package k8s

import (
	"errors"
	"math/rand"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// PodStatus should be defined elsewhere in your codebase
// type PodStatus struct {
//     PodName   string
//     PodIP     string
//     Status    string // "idle", "busy", etc.
//     // ... other fields ...
// }

type IdleFirstSelector struct {
	// You can add fields if you want to track state
}

func NewIdleFirstSelector() *IdleFirstSelector {
	return &IdleFirstSelector{}
}

// Select returns the index of the pod to use, or -1 if none found.
// It implements the idle-first logic described in your prompt.
func (s *IdleFirstSelector) Select(
	podStatuses []PodStatus,
	addresses []corev1.EndpointAddress,
	functionName, namespace string,
	checkPodAvailable func(podIP string) bool,
	scaleUpFunc func() error,
	refreshFunc func() ([]PodStatus, []corev1.EndpointAddress),
) (int, error) {
	// 1. Try idle pods up to 3 times
	idlePods := filterIdlePods(podStatuses)
	tryCount := 0
	for tryCount < 3 && len(idlePods) > 0 {
		selected := idlePods[rand.Intn(len(idlePods))]
		if checkPodAvailable(selected.PodIP) {
			// Find index in addresses
			for i, addr := range addresses {
				if addr.IP == selected.PodIP {
					return i, nil
				}
			}
		}
		// Remove and retry
		idlePods = removePodFromList(idlePods, selected.PodIP)
		tryCount++
	}

	// 2. Scale up if no idle pods
	if err := scaleUpFunc(); err != nil {
		return -1, err
	}

	// Wait for new pod logic (polling)
	timeout := time.After(30 * time.Second)
	tick := time.Tick(1 * time.Second)
	for {
		select {
		case <-timeout:
			return -1, errors.New("timed out waiting for new pod")
		case <-tick:
			// Refresh pod statuses and addresses
			podStatuses, addresses = refreshFunc()
			idlePods = filterIdlePods(podStatuses)
			if len(idlePods) > 0 {
				// Prefer any idle pod that was already in the cache (not the new one)
				for _, pod := range idlePods {
					if checkPodAvailable(pod.PodIP) {
						for i, addr := range addresses {
							if addr.IP == pod.PodIP {
								return i, nil
							}
						}
					}
				}
			}
		}
	}
}

// Helper to filter idle pods
func filterIdlePods(pods []PodStatus) []PodStatus {
	idle := make([]PodStatus, 0, len(pods))
	for _, pod := range pods {
		if pod.Status == "idle" {
			idle = append(idle, pod)
		}
	}
	return idle
}

// Remove a pod from the list by PodIP
func removePodFromList(pods []PodStatus, podIP string) []PodStatus {
	out := make([]PodStatus, 0, len(pods))
	for _, p := range pods {
		if p.PodIP != podIP {
			out = append(out, p)
		}
	}
	return out
}
