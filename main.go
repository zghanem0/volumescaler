package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	// Kubernetes client-go imports
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// VolumeScaler represents the custom resource specification
type VolumeScaler struct {
	Spec struct {
		PVCName   string `json:"pvcName"`
		Threshold string `json:"threshold"` // e.g. "80%"
		Scale     string `json:"scale"`     // e.g. "20%"
		MaxSize   string `json:"maxSize"`   // e.g. "100Gi"
	} `json:"spec"`
	Status struct {
		ScaledAt       string `json:"scaledAt,omitempty"`
		ReachedMaxSize bool   `json:"reachedMaxSize,omitempty"`
	} `json:"status,omitempty"`
}

func convertToGi(sizeStr string) (float64, error) {
	// Examples: "5Gi", "100Gi"
	// Extract numeric and unit part
	var numberStr, unitStr string
	for i, r := range sizeStr {
		if r < '0' || r > '9' {
			numberStr = sizeStr[:i]
			unitStr = sizeStr[i:]
			break
		}
	}
	if numberStr == "" && unitStr == "" {
		// Entire string was numbers
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

func runKubectl(args ...string) ([]byte, error) {
	cmd := exec.Command("kubectl", args...)
	return cmd.CombinedOutput()
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

				// Get PVC details
				pvc, err := clientset.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvcName, metav1.GetOptions{})
				if err != nil {
					fmt.Printf("Error fetching PVC details for %s in namespace %s.\n", pvcName, namespace)
					continue
				}

				currentPVCSizeRaw := pvc.Spec.Resources.Requests.Storage().String()
				currentPVCSize, err := convertToGi(currentPVCSizeRaw)
				if err != nil {
					fmt.Printf("Error converting PVC size: %v\n", err)
					continue
				}

				// Get VolumeScaler details via kubectl (dynamic client or custom client could be used in a real controller)
				vsRaw, err := runKubectl("get", "volumescaler", pvcName, "-n", namespace, "-o", "json")
				if err != nil || len(vsRaw) == 0 {
					fmt.Printf("No VolumeScaler found for PVC: %s in namespace %s.\n", pvcName, namespace)
					continue
				}

				var scaler VolumeScaler
				if err := json.Unmarshal(vsRaw, &scaler); err != nil {
					fmt.Printf("Error parsing VolumeScaler JSON: %v\n", err)
					continue
				}

				thresholdStr := strings.TrimSuffix(scaler.Spec.Threshold, "%")
				threshold, err := strconv.Atoi(thresholdStr)
				if err != nil {
					fmt.Printf("Invalid threshold in VolumeScaler %s: %v\n", pvcName, err)
					continue
				}

				scaleStr := strings.TrimSuffix(scaler.Spec.Scale, "%")
				scaleVal, err := strconv.ParseFloat(scaleStr, 64)
				if err != nil {
					fmt.Printf("Invalid scale in VolumeScaler %s: %v\n", pvcName, err)
					continue
				}

				maxSizeGi, err := convertToGi(scaler.Spec.MaxSize)
				if err != nil {
					fmt.Printf("Invalid maxSize in VolumeScaler %s: %v\n", pvcName, err)
					continue
				}

				// Check if current at or beyond max
				if currentPVCSize >= maxSizeGi {
					if !scaler.Status.ReachedMaxSize {
						fmt.Printf("PVC '%s' in namespace '%s' has reached its maxSize of %.2fGi.\n", pvcName, namespace, maxSizeGi)
						_, err := runKubectl("patch", "volumescaler", pvcName, "-n", namespace, "--type=merge", "-p", "{\"status\": {\"reachedMaxSize\": true}}")
						if err != nil {
							fmt.Printf("Error patching VolumeScaler status: %v\n", err)
						}
					}
					continue
				}

				// Construct mount path (assuming CSI volume mount structure)
				mountPath := fmt.Sprintf("/var/lib/kubelet/pods/%s/volumes/kubernetes.io~csi/pvc-%s/mount", podUID, pvc.UID)
				if _, err := os.Stat(mountPath); os.IsNotExist(err) {
					fmt.Printf("Error: Mount path does not exist for PVC: %s.\n", pvcName)
					continue
				}

				// Get disk usage
				dfOutput, err := exec.Command("df", mountPath).CombinedOutput()
				if err != nil {
					fmt.Printf("Error running df for PVC %s: %v\n", pvcName, err)
					continue
				}
				lines := strings.Split(strings.TrimSpace(string(dfOutput)), "\n")
				if len(lines) < 2 {
					fmt.Printf("Error: Unable to parse df output for PVC: %s.\n", pvcName)
					continue
				}
				fields := strings.Fields(lines[1])
				if len(fields) < 4 {
					fmt.Printf("Error: df output not as expected for PVC: %s.\n", pvcName)
					continue
				}

				usedBlocksStr := fields[2]
				usedBlocks, err := strconv.ParseFloat(usedBlocksStr, 64)
				if err != nil {
					fmt.Printf("Error parsing used blocks for PVC %s: %v\n", pvcName, err)
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
								fmt.Printf("PVC: %s was recently scaled. Skipping.\n", pvcName)
								continue
							}
						}
					}

					// Calculate new size
					incrementSize := currentPVCSize * (scaleVal / 100.0)
					newSize := currentPVCSize + incrementSize
					if newSize > maxSizeGi {
						newSize = maxSizeGi
						fmt.Printf("New size would exceed maxSize. Setting new size to %.0fGi.\n", newSize)
					}

					if newSize == currentPVCSize {
						fmt.Printf("PVC '%s' is already at size %.0fGi. No patch needed.\n", pvcName, currentPVCSize)
						continue
					}

					newSizeStr := fmt.Sprintf("%.0fGi", newSize)
					patchData := fmt.Sprintf(`{"spec":{"resources":{"requests":{"storage":"%s"}}}}`, newSizeStr)
					_, err = runKubectl("patch", "pvc", pvcName, "-n", namespace, "--type=merge", "-p", patchData)
					if err != nil {
						fmt.Printf("Failed to scale PVC: %s, Namespace: %s. Error: %v\n", pvcName, namespace, err)
						continue
					}

					fmt.Printf("Scaled PVC: %s, Namespace: %s, New Size: %s\n", pvcName, namespace, newSizeStr)
					currentTime := time.Now().UTC().Format(time.RFC3339)
					_, err = runKubectl("patch", "volumescaler", pvcName, "-n", namespace, "--type=merge", "-p", fmt.Sprintf("{\"status\":{\"scaledAt\":\"%s\"}}", currentTime))
					if err != nil {
						fmt.Printf("Error patching VolumeScaler scaledAt: %v\n", err)
					}

					// Check proximity to max
					difference := maxSizeGi - newSize
					if difference <= 1 && !scaler.Status.ReachedMaxSize {
						fmt.Printf("PVC '%s' is at or near maxSize (%.0fGi). Marking as reached max.\n", pvcName, maxSizeGi)
						_, err := runKubectl("patch", "volumescaler", pvcName, "-n", namespace, "--type=merge", "-p", "{\"status\":{\"reachedMaxSize\":true}}")
						if err != nil {
							fmt.Printf("Error patching VolumeScaler reachedMaxSize: %v\n", err)
						}
					}
				} else {
					fmt.Printf("PVC: %s, Namespace: %s, PVC Size: %.0fGi, Utilization: %.2fGi (%.0f%%) and below Threshold (%d%%). No scaling needed.\n",
						pvcName, namespace, currentPVCSize, usedGi, utilization, threshold)
				}
			}
		}

		time.Sleep(60 * time.Second)
	}
}
