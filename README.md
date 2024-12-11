# VolumeScaler

**VolumeScaler** is a Kubernetes controller that automatically scales PersistentVolumeClaims (PVCs) when a specified utilization threshold is reached. It is implemented as a DaemonSet running on each node, monitoring disk usage for PVCs mounted by pods on that node, and dynamically adjusting the PVC request size up to a defined maximum.

## Features

- **Dynamic PVC Expansion**: Automatically increase PVC size once utilization passes a configurable threshold.
- **Custom Resource Definition**: Define scaling rules using a `VolumeScaler` CR, specifying the target PVC, threshold, scale percentage, and maximum allowed size.
- **Automatic Max-Size Detection**: Once the PVC reaches the defined max size, the controller updates the `VolumeScaler` status to indicate that no further scaling will occur.
- **Backoff for Scaling**: Implements a cooldown period to avoid frequent re-scaling within a short timeframe.

## Prerequisites

- Kubernetes cluster with a storage provider that supports online volume expansion (e.g., EBS CSI driver)
- A StorageClass that enables volume expansion
- RBAC permissions allowing the VolumeScaler DaemonSet to list pods, PVCs, and VolumeScaler CRs, and patch their status

## Installation

1. **Install the CRD and VolumeScaler Controler**:
   ```bash
   kubectl apply -f volumescaler.yaml
   ```

2. **Deploy pod-data-generator for testing**:
   ```bash
   kubectl apply -f test-pod-data-generator.yaml
   ```

## How It Works

### 1. VolumeScaler 
Define a VolumeScaler custom resource in the same namespace as the PVC. It should specify:
- `pvcName`: The name of the PVC to monitor
- `threshold`: Utilization threshold in percentage (e.g., 70%)
- `scale`: The percentage increase in PVC size when threshold is exceeded (e.g., 30%)
- `maxSize`: The maximum PVC size (e.g., 100Gi)

### 2. Monitoring Utilization
The DaemonSet runs on every node, listing pods running on that node. For each pod volume that references a PVC, it checks the disk usage using. The usage is compared against the threshold from the matching VolumeScaler resource.

### 3. Scaling PVC
If the utilization is above the threshold and cooldown conditions are met (not scaled recently), the controller:
- Calculates the new size based on the scale percentage
- Ensures it does not exceed maxSize
- Patches the PVC to request the new size
- Updates the VolumeScaler status with the time of the last scale

### 4. Reaching Max Size
When the PVC reaches or is near the maxSize, the VolumeScaler status is updated to indicate `reachedMaxSize`. No further scaling is performed once this is true.

## Example VolumeScaler

```yaml
apiVersion: zghanem.aws/v1
kind: VolumeScaler
metadata:
  name: example-pvc
  namespace: default
spec:
  pvcName: example-pvc
  threshold: "70%"
  scale: "30%"
  maxSize: "10Gi"
```

## Troubleshooting

- **Permissions Issues**: 
  If you see "forbidden" errors when patching VolumeScaler status, ensure the RBAC roles allow patch on volumescalers/status.

- **Unknown Field "status"**: 
  Make sure the CRD schema includes a status property and subresources.status is enabled. Update the CRD if necessary.

- **kubectl not found / Not Needed**: 
  The controller uses the Kubernetes client-go libraries directly. No need for kubectl inside the container image once fully migrated to client-go interactions.

## Contributing

Contributions are welcome! Please open issues or pull requests on the repository for bug fixes, new features, or documentation improvements.

## License

This project is licensed under the MIT License. See [LICENSE](LICENSE) for details.