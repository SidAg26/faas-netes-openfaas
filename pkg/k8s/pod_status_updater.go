// SA - This is the PodStatusUpdater interface implementation for managing pod statuses in a Kubernetes environment.
// In faas-netes/pkg/k8s/pod_status_updater.go (new file)

package k8s

import (
	"log"
	"time"

	providertypes "github.com/openfaas/faas-provider/types"
)

// Implement the PodStatusUpdater interface
func (l *FunctionLookup) MarkPodBusy(podName, podIP string) error {
	if status, exists := l.podStatusCache.Get(podName, podIP); exists {
		l.podStatusCache.Set(podName, "busy", podIP, status.Function, status.Namespace, status.MaxInflight) // SA - add maxInflight
	}
	return nil
}

func (l *FunctionLookup) MarkPodIdle(podName, podIP string) error {
	if status, exists := l.podStatusCache.Get(podName, podIP); exists {
		log.Printf("Marking pod %s as idle", podName)
		l.podStatusCache.Set(podName, "idle", podIP, status.Function, status.Namespace, status.MaxInflight) // SA - add maxInflight
	}
	return nil
}

func (l *FunctionLookup) GetPodStatus(podName, podIP string) (providertypes.PodStatus, bool) {
	status, exists := l.podStatusCache.Get(podName, podIP)
	if !exists {
		return providertypes.PodStatus{}, false
	}

	return providertypes.PodStatus{
		Status:    status.Status,
		Timestamp: status.Timestamp.Format(time.RFC3339),
		PodIP:     status.PodIP,
		Function:  status.Function,
		Namespace: status.Namespace,
	}, true
}

// SA - This function retrieves the status of all pods for a specific function in a given namespace.
// It returns a slice of PodStatus objects containing the status, timestamp, pod IP, function name, and namespace.
func (l *FunctionLookup) GetPodStatusByFunction(functionName string, namespace string) ([]providertypes.PodStatus, error) {
	statuses := l.podStatusCache.GetByFunction(functionName, namespace)
	result := make([]providertypes.PodStatus, 0, len(statuses))

	for _, status := range statuses {
		if status.Namespace == namespace {
			result = append(result, providertypes.PodStatus{
				Status:    status.Status,
				Timestamp: status.Timestamp.Format(time.RFC3339),
				PodIP:     status.PodIP,
				Function:  status.Function,
				Namespace: status.Namespace,
				PodName:   status.PodName,
			})
		}
	}

	return result, nil // Return a copy of the slice to avoid exposing the internal cache directly
}
