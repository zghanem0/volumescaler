package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types" // for types.MergePatchType

	// Kubernetes client-go imports
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// VolumeScaler represents the custom resource specification
type VolumeScaler struct {
	Spec struct {
		PVCName   string `json:"pvcName"`
		Threshold string `json:"threshold"`
		Scale     string `json:"scale"`
		MaxSize   string `json:"maxSize"`
	} `json:"spec"`
	Status struct {
		ScaledAt       string `json:"scaledAt,omitempty"`
		ReachedMaxSize bool   `json:"reachedMaxSize,omitempty"`
	} `json:"status,omitempty"`
}

func convertToGi(sizeStr string) (float64, error) {
	var numberStr, unitStr string
	for i, r := range sizeStr {
		if r < '0' || r > '9' {
			numberStr = sizeStr[:i]
			unitStr = sizeStr[i:]
			break
		}
	}
	if numberStr == "" && unitStr == "" {
		numberStr = sizeStr
		unitStr = "Gi"
	}

	number, err := strconv.ParseFloat(numberStr, 64)
	if err != nil {
		return 0, err
	}

	switch unitStr {
	case "Gi":
		return number, nil
	case "Mi":
		return number / 1024, nil
	case "Ti":
		return number * 1024, nil
	default:
		return number, nil
	}
}

func main() {
	nodeName := os.Getenv("NODE_NAME_ENV")
	if nodeName == "" {
		fmt.Println("NODE_NAME_ENV not set, exiting.")
		os.Exit(1)
	}

	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err.Error())
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	dynClient, err := dynamic.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	gvr := schema.GroupVersionResource{
		Group:    "zghanem.aws",
		Version:  "v1",
		Resource: "volumescalers",
	}

	for {
		ctx := context.Background()

		pods, err := clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{
			FieldSelector: "spec.nodeName=" + nodeName,
		})
		if err != nil {
			fmt.Printf("Error fetching pods for node %s. Retrying...\n", nodeName)
			time.Sleep(60 * time.Second)
			continue
		}

		for _, pod := range pods.Items {
			namespace := pod.Namespace
			podUID := string(pod.UID)

			for _, vol := range pod.Spec.Volumes {
				if vol.PersistentVolumeClaim == nil {
					continue
				}
				pvcName := vol.PersistentVolumeClaim.ClaimName

				pvc, err := clientset.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvcName, metav1.GetOptions{})
				if err != nil {
					fmt.Printf("Error fetching PVC details for %s in namespace %s: %v\n", pvcName, namespace, err)
					continue
				}

				currentPVCSizeRaw := pvc.Spec.Resources.Requests.Storage().String()
				currentPVCSize, err := convertToGi(currentPVCSizeRaw)
				if err != nil {
					fmt.Printf("Error converting PVC size (%s) for PVC %s in namespace %s: %v\n", currentPVCSizeRaw, pvcName, namespace, err)
					continue
				}

				vsUnstructuredList, err := dynClient.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{})
				if err != nil {
					fmt.Printf("Error listing VolumeScalers in namespace %s: %v\n", namespace, err)
					fmt.Printf("No VolumeScaler found for PVC: %s in namespace %s.\n", pvcName, namespace)
					continue
				}

				if len(vsUnstructuredList.Items) == 0 {
					fmt.Printf("No VolumeScalers found in namespace %s.\n", namespace)
					fmt.Printf("No VolumeScaler found for PVC: %s in namespace %s.\n", pvcName, namespace)
					continue
				}

				var scaler *VolumeScaler
				var scalerName string
				for _, u := range vsUnstructuredList.Items {
					vsObj := &VolumeScaler{}
					if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, vsObj); err != nil {
						fmt.Printf("Error converting unstructured to VolumeScaler: %v\n", err)
						continue
					}
					if vsObj.Spec.PVCName == pvcName {
						scaler = vsObj
						scalerName = u.GetName()
						break
					}
				}

				if scaler == nil {
					fmt.Printf("No VolumeScaler found for PVC: %s in namespace %s after checking all.\n", pvcName, namespace)
					continue
				}

				thresholdStr := strings.TrimSuffix(scaler.Spec.Threshold, "%")
				threshold, err := strconv.Atoi(thresholdStr)
				if err != nil {
					fmt.Printf("Invalid threshold (%s) in VolumeScaler for PVC %s in namespace %s: %v\n", scaler.Spec.Threshold, pvcName, namespace, err)
					continue
				}

				scaleStr := strings.TrimSuffix(scaler.Spec.Scale, "%")
				scaleVal, err := strconv.ParseFloat(scaleStr, 64)
				if err != nil {
					fmt.Printf("Invalid scale (%s) in VolumeScaler for PVC %s in namespace %s: %v\n", scaler.Spec.Scale, pvcName, namespace, err)
					continue
				}

				maxSizeGi, err := convertToGi(scaler.Spec.MaxSize)
				if err != nil {
					fmt.Printf("Invalid maxSize (%s) in VolumeScaler for PVC %s in namespace %s: %v\n", scaler.Spec.MaxSize, pvcName, namespace, err)
					continue
				}

				// Check if current at or beyond max
				if currentPVCSize >= maxSizeGi {
					if !scaler.Status.ReachedMaxSize {
						fmt.Printf("PVC '%s' in namespace '%s' has reached its maxSize of %.2fGi.\n", pvcName, namespace, maxSizeGi)
						patch := []byte(`{"status": {"reachedMaxSize": true}}`)
						_, err := dynClient.Resource(gvr).Namespace(namespace).Patch(ctx, scalerName, types.MergePatchType, patch, metav1.PatchOptions{}, "status")
						if err != nil {
							fmt.Printf("Error patching VolumeScaler status: %v\n", err)
						}
					}
					continue
				}

				mountPath := fmt.Sprintf("/var/lib/kubelet/pods/%s/volumes/kubernetes.io~csi/pvc-%s/mount", podUID, pvc.UID)
				if _, err := os.Stat(mountPath); os.IsNotExist(err) {
					fmt.Printf("Mount path does not exist for PVC: %s at %s.\n", pvcName, mountPath)
					continue
				}

				dfOutput, err := exec.Command("df", mountPath).CombinedOutput()
				if err != nil {
					fmt.Printf("Error running df for PVC %s: %v\n", pvcName, err)
					continue
				}
				lines := strings.Split(strings.TrimSpace(string(dfOutput)), "\n")
				if len(lines) < 2 {
					fmt.Printf("Unable to parse df output for PVC: %s.\n", pvcName)
					fmt.Printf("DF Output: %s\n", string(dfOutput))
					continue
				}
				fields := strings.Fields(lines[1])
				if len(fields) < 4 {
					fmt.Printf("df output not as expected for PVC: %s. Fields: %v\n", pvcName, fields)
					continue
				}

				usedBlocksStr := fields[2]
				usedBlocks, err := strconv.ParseFloat(usedBlocksStr, 64)
				if err != nil {
					fmt.Printf("Error parsing used blocks (%s) for PVC %s: %v\n", usedBlocksStr, pvcName, err)
					continue
				}

				usedGi := usedBlocks / 1024 / 1024
				utilization := (usedGi / currentPVCSize) * 100

				if int(utilization) >= threshold {
					lastScaledTime := scaler.Status.ScaledAt
					if lastScaledTime != "" {
						lt, err := time.Parse(time.RFC3339, lastScaledTime)
						if err == nil {
							if time.Now().UTC().Before(lt.Add(6 * time.Hour)) {
								fmt.Printf("PVC: %s was recently scaled at %s. Skipping.\n", pvcName, lastScaledTime)
								continue
							}
						} else {
							fmt.Printf("Error parsing scaledAt time (%s) for PVC %s: %v\n", lastScaledTime, pvcName, err)
						}
					}

					incrementSize := currentPVCSize * (scaleVal / 100.0)
					newSize := currentPVCSize + incrementSize
					if newSize > maxSizeGi {
						fmt.Printf("New size (%.2fGi) would exceed maxSize (%.2fGi) for PVC %s. Setting to maxSize.\n", newSize, maxSizeGi, pvcName)
						newSize = maxSizeGi
					}

					if newSize == currentPVCSize {
						fmt.Printf("PVC '%s' is already at size %.0fGi. No patch needed.\n", pvcName, currentPVCSize)
						continue
					}

					newSizeStr := fmt.Sprintf("%.0fGi", newSize)
					pvcPatch := []byte(fmt.Sprintf(`{"spec":{"resources":{"requests":{"storage":"%s"}}}}`, newSizeStr))
					_, err = clientset.CoreV1().PersistentVolumeClaims(namespace).Patch(ctx, pvcName, types.MergePatchType, pvcPatch, metav1.PatchOptions{})
					if err != nil {
						fmt.Printf("Failed to scale PVC: %s, Namespace: %s. Error: %v\n", pvcName, namespace, err)
						continue
					}

					fmt.Printf("Scaled PVC: %s, Namespace: %s, New Size: %s\n", pvcName, namespace, newSizeStr)
					currentTime := time.Now().UTC().Format(time.RFC3339)
					vsPatchScaledAt := []byte(fmt.Sprintf(`{"status":{"scaledAt":"%s"}}`, currentTime))
					_, err = dynClient.Resource(gvr).Namespace(namespace).Patch(ctx, scalerName, types.MergePatchType, vsPatchScaledAt, metav1.PatchOptions{}, "status")
					if err != nil {
						fmt.Printf("Error patching VolumeScaler scaledAt: %v\n", err)
					}

					difference := maxSizeGi - newSize
					if difference <= 1 && !scaler.Status.ReachedMaxSize {
						fmt.Printf("PVC '%s' is at or near maxSize (%.0fGi). Marking as reached max.\n", pvcName, maxSizeGi)
						vsPatchReachedMax := []byte(`{"status":{"reachedMaxSize":true}}`)
						_, err := dynClient.Resource(gvr).Namespace(namespace).Patch(ctx, scalerName, types.MergePatchType, vsPatchReachedMax, metav1.PatchOptions{}, "status")
						if err != nil {
							fmt.Printf("Error patching VolumeScaler reachedMaxSize: %v\n", err)
						}
					}
				} else {
					fmt.Printf("PVC: %s, Namespace: %s, PVC Size: %.0fGi, Used: %.2fGi (%.0f%%), Threshold: %d%%. No scaling needed.\n",
						pvcName, namespace, currentPVCSize, usedGi, utilization, threshold)
				}
			}
		}

		time.Sleep(60 * time.Second)
	}
}
