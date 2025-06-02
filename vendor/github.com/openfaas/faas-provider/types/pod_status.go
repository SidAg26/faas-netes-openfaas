// SA - This is the PodStatusUpdater interface and
// it provides methods to mark pods as busy or idle, and to get the status of a pod.
// In faas-provider/types/pod_status.go (new file)

package types

// PodStatus represents the status of a pod
type PodStatus struct {
	PodName   string `json:"podName"`
	Status    string `json:"status"`
	Timestamp string `json:"timestamp"`
	PodIP     string `json:"podIP"`
	Function  string `json:"function"`
	Namespace string `json:"namespace"`
}

// PodStatusUpdater defines the interface for updating pod status
type PodStatusUpdater interface {
	MarkPodBusy(podName, podIP string) error
	MarkPodIdle(podName, podIP string) error
	GetPodStatus(podName, podIP string) (PodStatus, bool)
}
