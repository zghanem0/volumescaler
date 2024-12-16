package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	// Logging
	"github.com/sirupsen/logrus"

	// Kubernetes client-go packages

	v1 "k8s.io/api/core/v1" // For EventSource and Event definitions
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	// "k8s.io/apimachinery/pkg/util/json"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
)

// VolumeScaler represents the custom resource specification
type VolumeScaler struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              VolumeScalerSpec   `json:"spec,omitempty"`
	Status            VolumeScalerStatus `json:"status,omitempty"`
}

type VolumeScalerSpec struct {
	PVCName   string `json:"pvcName"`
	Threshold string `json:"threshold"` // e.g., "70%"
	Scale     string `json:"scale"`     // e.g., "30%"
	MaxSize   string `json:"maxSize"`   // e.g., "100Gi"
}

type VolumeScalerStatus struct {
	ScaledAt       string `json:"scaledAt,omitempty"`
	ReachedMaxSize bool   `json:"reachedMaxSize,omitempty"`
}

// Controller structure
type Controller struct {
	clientset       kubernetes.Interface
	dynClient       dynamic.Interface
	volumeScalerGVR schema.GroupVersionResource
	workqueue       workqueue.RateLimitingInterface
	informer        cache.SharedIndexInformer
	recorder        record.EventRecorder
	logger          *logrus.Entry
}

func main() {
	// Initialize logger
	logger := logrus.New()
	logger.SetFormatter(&logrus.JSONFormatter{})
	entry := logrus.NewEntry(logger)

	// Get node name and pod name from environment variables
	nodeName := os.Getenv("NODE_NAME_ENV")
	if nodeName == "" {
		entry.Fatal("NODE_NAME_ENV not set, exiting.")
	}

	podName := os.Getenv("POD_NAME")
	if podName == "" {
		entry.Fatal("POD_NAME environment variable not set, cannot proceed with leader election.")
	}

	// In-cluster configuration
	config, err := rest.InClusterConfig()
	if err != nil {
		entry.Fatalf("Error creating in-cluster config: %v", err)
	}

	// Create Kubernetes clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		entry.Fatalf("Error creating Kubernetes clientset: %v", err)
	}

	// Create dynamic client
	dynClient, err := dynamic.NewForConfig(config)
	if err != nil {
		entry.Fatalf("Error creating dynamic client: %v", err)
	}

	// Define GVR for VolumeScaler CR
	gvr := schema.GroupVersionResource{
		Group:    "zghanem.aws",
		Version:  "v1",
		Resource: "volumescalers",
	}

	// Initialize event broadcaster
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartStructuredLogging(0)
	eventBroadcaster.StartRecordingToSink(&v1.EventSinkImpl{
		Interface: clientset.CoreV1().Events(""),
	})
	recorder := eventBroadcaster.NewRecorder(
		runtime.NewScheme(),
		v1.EventSource{Component: "volumescaler-controller"},
	)

	// Create controller instance
	controller := NewController(clientset, dynClient, gvr, recorder, entry)

	// Setup leader election configuration
	leaderElectionConfig := leaderelection.LeaderElectionConfig{
		Lock: &resourcelock.LeaseLock{
			LeaseMeta: metav1.ObjectMeta{
				Name:      "volumescaler-controller-lock",
				Namespace: "kube-system",
			},
			LockConfig: resourcelock.ResourceLockConfig{
				Identity:      podName, // Unique identity for leader election
				ClientConfig:  rest.CopyConfig(config),
				EventRecorder: recorder, // Use the event recorder
			},
		},
		ReleaseOnCancel: true,
		LeaseDuration:   15 * time.Second,
		RenewDeadline:   10 * time.Second,
		RetryPeriod:     2 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				entry.Infof("Became the leader, starting controller")
				if err := controller.Run(ctx, 2); err != nil {
					entry.Fatalf("Controller failed: %v", err)
				}
			},
			OnStoppedLeading: func() {
				entry.Fatal("Leader election lost, shutting down")
			},
			OnNewLeader: func(identity string) {
				if identity == podName {
					// This pod just became the leader
					return
				}
				entry.Infof("New leader elected: %s", identity)
			},
		},
	}

	// Handle graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)
		sig := <-sigChan
		entry.Infof("Received signal %s, initiating shutdown", sig)
		cancel()
	}()

	// Start leader election
	entry.Info("Starting leader election")
	if err := leaderelection.RunOrDie(ctx, leaderElectionConfig); err != nil {
		entry.Fatalf("Leader election failed: %v", err)
	}

	entry.Info("Shutting down gracefully")
}

// NewController initializes a new Controller
func NewController(clientset kubernetes.Interface, dynClient dynamic.Interface, gvr schema.GroupVersionResource, recorder record.EventRecorder, logger *logrus.Entry) *Controller {
	// Create informer for VolumeScaler CR
	informer := cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				return dynClient.Resource(gvr).Namespace(metav1.NamespaceAll).List(context.TODO(), options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return dynClient.Resource(gvr).Namespace(metav1.NamespaceAll).Watch(context.TODO(), options)
			},
		},
		&unstructured.Unstructured{},
		0, // No resync
		cache.Indexers{},
	)

	// Create workqueue
	workqueue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())

	// Set up event handlers
	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err == nil {
				workqueue.Add(key)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(newObj)
			if err == nil {
				workqueue.Add(key)
			}
		},
		DeleteFunc: func(obj interface{}) {
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err == nil {
				workqueue.Add(key)
			}
		},
	})

	return &Controller{
		clientset:       clientset,
		dynClient:       dynClient,
		volumeScalerGVR: gvr,
		workqueue:       workqueue,
		informer:        informer,
		recorder:        recorder,
		logger:          logger,
	}
}

// Run starts the controller
func (c *Controller) Run(ctx context.Context, workers int) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	// Start informer
	go c.informer.Run(ctx.Done())

	// Wait for cache sync
	if !cache.WaitForCacheSync(ctx.Done(), c.informer.HasSynced) {
		c.logger.Error("Timed out waiting for caches to sync")
		return fmt.Errorf("cache sync failed")
	}

	c.logger.Info("Caches are synced. Starting workers.")

	// Start workers
	for i := 0; i < workers; i++ {
		go wait.Until(c.runWorker, time.Second, ctx.Done())
	}

	c.logger.Info("Workers started.")

	// Wait until context is done
	<-ctx.Done()
	c.logger.Info("Controller is shutting down gracefully")
	return nil
}

func (c *Controller) runWorker() {
	for c.processNextItem() {
	}
}

func (c *Controller) processNextItem() bool {
	// Get next item from workqueue
	key, shutdown := c.workqueue.Get()
	if shutdown {
		return false
	}

	// Always mark the item as done
	defer c.workqueue.Done(key)

	// Process the key
	err := c.syncHandler(key.(string))
	if err != nil {
		// Requeue the item rate limited
		c.workqueue.AddRateLimited(key)
		c.logger.Errorf("Error syncing '%s': %v, requeuing", key, err)
		return true
	}

	// No error, forget the key
	c.workqueue.Forget(key)
	c.logger.Infof("Successfully synced '%s'", key)
	return true
}

// syncHandler processes a VolumeScaler resource
func (c *Controller) syncHandler(key string) error {
	// Split the key into namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		c.logger.Errorf("Invalid resource key: %s", key)
		return nil
	}

	// Get the VolumeScaler resource
	unstructuredObj, err := c.dynClient.Resource(c.volumeScalerGVR).Namespace(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			// Resource no longer exists
			c.logger.Infof("VolumeScaler '%s' in namespace '%s' no longer exists", name, namespace)
			return nil
		}
		return err
	}

	// Convert unstructured to VolumeScaler struct
	vs := &VolumeScaler{}
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredObj.Object, vs)
	if err != nil {
		c.logger.Errorf("Error converting unstructured to VolumeScaler: %v", err)
		return err
	}

	// Fetch the associated PVC
	pvc, err := c.clientset.CoreV1().PersistentVolumeClaims(namespace).Get(context.TODO(), vs.Spec.PVCName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			c.logger.Warnf("PVC '%s' in namespace '%s' not found", vs.Spec.PVCName, namespace)
			return nil
		}
		return err
	}

	// Get current PVC size in Gi
	currentPVCSizeRaw := pvc.Spec.Resources.Requests.Storage().String()
	currentPVCSize, err := convertToGi(currentPVCSizeRaw)
	if err != nil {
		c.logger.Errorf("Error converting PVC size (%s) for PVC %s in namespace %s: %v", currentPVCSizeRaw, vs.Spec.PVCName, namespace, err)
		return err
	}

	// Get threshold
	thresholdStr := strings.TrimSuffix(vs.Spec.Threshold, "%")
	threshold, err := strconv.Atoi(thresholdStr)
	if err != nil {
		c.logger.Errorf("Invalid threshold (%s) in VolumeScaler for PVC %s in namespace %s: %v", vs.Spec.Threshold, vs.Spec.PVCName, namespace, err)
		return err
	}

	// Get scale
	scaleStr := strings.TrimSuffix(vs.Spec.Scale, "%")
	scaleVal, err := strconv.ParseFloat(scaleStr, 64)
	if err != nil {
		c.logger.Errorf("Invalid scale (%s) in VolumeScaler for PVC %s in namespace %s: %v", vs.Spec.Scale, vs.Spec.PVCName, namespace, err)
		return err
	}

	// Get maxSize in Gi
	maxSizeGi, err := convertToGi(vs.Spec.MaxSize)
	if err != nil {
		c.logger.Errorf("Invalid maxSize (%s) in VolumeScaler for PVC %s in namespace %s: %v", vs.Spec.MaxSize, vs.Spec.PVCName, namespace, err)
		return err
	}

	// Check if PVC has reached maxSize
	if currentPVCSize >= maxSizeGi {
		if !vs.Status.ReachedMaxSize {
			// Patch status to mark reachedMaxSize
			patch := map[string]interface{}{
				"status": map[string]interface{}{
					"reachedMaxSize": true,
				},
			}
			patchBytes, _ := json.Marshal(patch)
			_, err = c.dynClient.Resource(c.volumeScalerGVR).Namespace(namespace).Patch(context.TODO(), name, types.MergePatchType, patchBytes, metav1.PatchOptions{}, "status")
			if err != nil {
				c.logger.Errorf("Error patching VolumeScaler status for '%s': %v", name, err)
				return err
			}
			c.logger.Infof("PVC '%s' in namespace '%s' has reached its maxSize of %.2fGi", vs.Spec.PVCName, namespace, maxSizeGi)
		}
		return nil
	}

	// Measure utilization
	utilization, err := measureUtilization(c, pvc, namespace)
	if err != nil {
		c.logger.Errorf("Error measuring utilization for PVC '%s' in namespace '%s': %v", vs.Spec.PVCName, namespace, err)
		return err
	}

	c.logger.Infof("PVC '%s' in namespace '%s' utilization: %.2f%% (Threshold: %d%%)", vs.Spec.PVCName, namespace, utilization, threshold)

	// Check if utilization meets or exceeds threshold
	if int(utilization) >= threshold {
		// Calculate new size
		incrementSize := currentPVCSize * (scaleVal / 100.0)
		newSize := currentPVCSize + incrementSize
		if newSize > maxSizeGi {
			c.logger.Infof("New size (%.2fGi) exceeds maxSize (%.2fGi) for PVC '%s'. Setting to maxSize.", newSize, maxSizeGi, vs.Spec.PVCName)
			newSize = maxSizeGi
		}

		if newSize <= currentPVCSize {
			// No need to patch if sizes are equal or smaller
			c.logger.Infof("PVC '%s' is already at size %.0fGi. No patch needed.", vs.Spec.PVCName, currentPVCSize)
			return nil
		}

		newSizeStr := fmt.Sprintf("%.0fGi", newSize)

		// Patch PVC to new size
		patch := map[string]interface{}{
			"spec": map[string]interface{}{
				"resources": map[string]interface{}{
					"requests": map[string]interface{}{
						"storage": newSizeStr,
					},
				},
			},
		}
		patchBytes, _ := json.Marshal(patch)
		_, err = c.clientset.CoreV1().PersistentVolumeClaims(namespace).Patch(context.TODO(), vs.Spec.PVCName, types.MergePatchType, patchBytes, metav1.PatchOptions{})
		if err != nil {
			c.logger.Errorf("Failed to scale PVC '%s' in namespace '%s': %v", vs.Spec.PVCName, namespace, err)
			return err
		}

		c.logger.Infof("Scaled PVC '%s' in namespace '%s' to new size: %s", vs.Spec.PVCName, namespace, newSizeStr)

		// Update VolumeScaler status
		currentTime := time.Now().UTC().Format(time.RFC3339)
		statusPatch := map[string]interface{}{
			"status": map[string]interface{}{
				"scaledAt": currentTime,
			},
		}
		statusPatchBytes, _ := json.Marshal(statusPatch)
		_, err = c.dynClient.Resource(c.volumeScalerGVR).Namespace(namespace).Patch(context.TODO(), name, types.MergePatchType, statusPatchBytes, metav1.PatchOptions{}, "status")
		if err != nil {
			c.logger.Errorf("Error patching VolumeScaler status for '%s': %v", name, err)
			return err
		}

		// Check if new size is at or near maxSize
		if newSize >= maxSizeGi {
			if !vs.Status.ReachedMaxSize {
				finalPatch := map[string]interface{}{
					"status": map[string]interface{}{
						"reachedMaxSize": true,
					},
				}
				finalPatchBytes, _ := json.Marshal(finalPatch)
				_, err = c.dynClient.Resource(c.volumeScalerGVR).Namespace(namespace).Patch(context.TODO(), name, types.MergePatchType, finalPatchBytes, metav1.PatchOptions{}, "status")
				if err != nil {
					c.logger.Errorf("Error patching reachedMaxSize for '%s': %v", name, err)
					return err
				}
				c.logger.Infof("PVC '%s' in namespace '%s' has reached its maxSize after scaling.", vs.Spec.PVCName, namespace)
			}
		}
	} else {
		// Below threshold, no scaling needed
		c.logger.Infof("PVC: %s, Namespace: %s, PVC Size: %.2fGi, Utilization: %.2f%%, Threshold: %d%%. No scaling needed.",
			vs.Spec.PVCName, namespace, currentPVCSize, utilization, threshold)
	}

	return nil
}

// measureUtilization calculates the storage utilization percentage for a given PVC
func measureUtilization(c *Controller, pvc *v1.PersistentVolumeClaim, namespace string) (float64, error) {
	// Locate the Pod using the PVC
	podList, err := c.clientset.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: fmt.Sprintf("volume.kubernetes.io/claim-name=%s", pvc.Name),
	})
	if err != nil {
		return 0, err
	}

	if len(podList.Items) == 0 {
		return 0, fmt.Errorf("no pods found using PVC '%s' in namespace '%s'", pvc.Name, namespace)
	}

	// Assuming the first pod is using the PVC
	pod := podList.Items[0]
	podUID := string(pod.UID)

	// Construct mount path (this might vary based on CSI driver)
	mountPath := fmt.Sprintf("/var/lib/kubelet/pods/%s/volumes/kubernetes.io~csi/pvc-%s/mount", podUID, pvc.UID)

	// Execute 'df' command with -B1 to get bytes
	cmd := exec.Command("df", "-B1", mountPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("error executing df command: %v, output: %s", err, string(output))
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) < 2 {
		return 0, fmt.Errorf("unexpected df output format: %s", string(output))
	}

	fields := strings.Fields(lines[1])
	if len(fields) < 5 {
		return 0, fmt.Errorf("df output does not contain enough fields: %v", fields)
	}

	usedBytesStr := fields[2]
	totalBytesStr := fields[1]

	usedBytes, err := strconv.ParseFloat(usedBytesStr, 64)
	if err != nil {
		return 0, fmt.Errorf("error parsing used bytes: %v", err)
	}

	totalBytes, err := strconv.ParseFloat(totalBytesStr, 64)
	if err != nil {
		return 0, fmt.Errorf("error parsing total bytes: %v", err)
	}

	utilization := (usedBytes / totalBytes) * 100
	return utilization, nil
}

// convertToGi parses a size string (like "5Gi") into a float representing Gi.
func convertToGi(sizeStr string) (float64, error) {
	sizeStr = strings.TrimSpace(sizeStr)
	if sizeStr == "" {
		return 0, fmt.Errorf("size string is empty")
	}

	var numberStr, unitStr string
	for i, r := range sizeStr {
		if (r < '0' || r > '9') && r != '.' {
			numberStr = sizeStr[:i]
			unitStr = strings.TrimSpace(sizeStr[i:])
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
		return 0, fmt.Errorf("error parsing number from size string '%s': %v", sizeStr, err)
	}

	switch strings.ToUpper(unitStr) {
	case "GIB", "GI":
		return number, nil
	case "MIB", "MI":
		return number / 1024, nil
	case "TIB", "TI":
		return number * 1024, nil
	default:
		// If unit is unrecognized, assume Gi and log a warning
		logrus.Warnf("Unrecognized unit '%s' in size string '%s'. Assuming Gi.", unitStr, sizeStr)
		return number, nil
	}
}
