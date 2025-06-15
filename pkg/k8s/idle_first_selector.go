package k8s

import (
	"context"
	"errors"
	"fmt"
	"log" // SA - Add the logging package
	"math"
	"math/rand"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1" // for Deployment envVariable access
	"k8s.io/client-go/kubernetes"
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
	clientset      *kubernetes.Clientset // Optional: if you need to interact with the Kubernetes API
	podStatusCache *PodStatusCache       // Cache for pod statuses
	functionLookup *FunctionLookup       // Optional: if you need to look up function details

	maxInflightGroup singleflight.Group // For deduplication of max_inflight requests
	maxInflightCache sync.Map           // Cache for max_inflight values map[string]int
	// Maps "namespace/functionName" to max_inflight value
}

func NewIdleFirstSelector(clientset *kubernetes.Clientset, podStatusCache *PodStatusCache, functionLookup *FunctionLookup) *IdleFirstSelector {
	return &IdleFirstSelector{
		clientset:      clientset,
		podStatusCache: podStatusCache,
		functionLookup: functionLookup,
	}
}

// Select returns the index of the pod to use, or -1 if none found.
// It implements the idle-first logic described in your prompt.
func (s *IdleFirstSelector) Select(
	addresses []corev1.EndpointAddress,
	functionName, namespace string,
) (int, error) {
	// // Helper to refresh addresses from Kubernetes Endpoints
	// refreshAddresses := func() []corev1.EndpointAddress {
	// 	endpoints, err := s.clientset.CoreV1().Endpoints(namespace).Get(context.TODO(), functionName, metav1.GetOptions{})
	// 	if err != nil || len(endpoints.Subsets) == 0 {
	// 		return nil
	// 	}
	// 	var all []corev1.EndpointAddress
	// 	for _, subset := range endpoints.Subsets {
	// 		all = append(all, subset.Addresses...)
	// 	}
	// 	return all
	// }
	// get the current max_inflight value for the function
	max_inflight, err := s.getFunctionMaxInflight(functionName, namespace)
	if err != nil {
		max_inflight = math.MaxInt32 // Default to maximum if not found allow infinite inflight requests
		log.Printf("Error getting max_inflight for function %s in namespace %s: %v", functionName, namespace, err)
	}

	// 1. Sync cache with endpoints (removes stale, adds new as idle)
	s.podStatusCache.PruneByAddresses(functionName, namespace, addresses, max_inflight)

	// 2. Try to find an idle pod and use it
	podStatuses := s.podStatusCache.GetByFunction(functionName, namespace)
	idlePods := filterIdlePodsForAddresses(podStatuses, addresses, max_inflight)
	tryCount := 0
	for tryCount < 3 && len(idlePods) > 0 {
		selected := idlePods[rand.Intn(len(idlePods))]
		if s.checkPodAvailable(selected.PodIP) {
			for i, addr := range addresses {
				if addr.IP == selected.PodIP {
					if s.podStatusCache.TryMarkPodBusy(selected.PodName, selected.PodIP) {
						s.functionLookup.MarkPodBusy(selected.PodName, selected.PodIP)
						// Found the index of the selected pod in the addresses list
						log.Printf("Selected pod %s at index %d", selected.PodName, i)
						return i, nil
					} else {
						// The pod was marked busy by another request, try again
						s.podStatusCache.PruneByAddresses(functionName, namespace, addresses, max_inflight)
						idlePods = filterIdlePodsForAddresses(podStatuses, addresses, max_inflight)
						continue
					}

				}
			}
		}
		idlePods = removePodFromList(idlePods, selected.PodIP) // Pods that are unavailable not the busy ones
		tryCount++
	}
	// --------------- REMOVING THE SCALING LOGIC FOR NOW ---------------
	// // 3. No idle pods: scale up and record old pod IPs
	// oldIPs := make(map[string]struct{}, len(addresses))
	// for _, addr := range addresses {
	// 	oldIPs[addr.IP] = struct{}{}
	// }
	// // If no idle pods found, scale up the deployment
	// log.Printf("No idle pods found for function %s in namespace %s, scaling up", functionName, namespace)
	// // if err := s.scaleUpFunc(functionName, namespace); err != nil {
	// // 	log.Printf("Error scaling up function %s in namespace %s: %v", functionName, namespace, err)
	// // 	return 0, err
	// // }

	// // 4. Wait for new pod logic (polling)
	// timeout := time.After(30 * time.Second)
	// tick := time.Tick(1 * time.Second)
	// var newPodIP string
	// for {
	// 	select {
	// 	case <-timeout:
	// 		return -1, errors.New("timed out waiting for new pod")
	// 	case <-tick:
	// 		addresses = refreshAddresses()
	// 		s.podStatusCache.PruneByAddresses(functionName, namespace, addresses, max_inflight)
	// 		podStatuses = s.podStatusCache.GetByFunction(functionName, namespace)
	// 		idlePods = filterIdlePodsForAddresses(podStatuses, addresses, max_inflight)

	// 		// Find new pod IP (not in oldIPs)
	// 		newPodIP = ""
	// 		for _, addr := range addresses {
	// 			if _, exists := oldIPs[addr.IP]; !exists {
	// 				newPodIP = addr.IP
	// 				break
	// 			}
	// 		}

	// 		// Prefer any other idle pod that is not the new pod
	// 		for _, pod := range idlePods {
	// 			if pod.PodIP != newPodIP && s.checkPodAvailable(pod.PodIP) {
	// 				s.functionLookup.MarkPodBusy(pod.PodName, pod.PodIP)
	// 				for i, addr := range addresses {
	// 					if addr.IP == pod.PodIP {
	// 						return i, nil
	// 					}
	// 				}
	// 			}
	// 		}

	// 		// If new pod is available, claim it for this request
	// 		if newPodIP != "" {
	// 			for _, pod := range idlePods {
	// 				if pod.PodIP == newPodIP && s.checkPodAvailable(newPodIP) {
	// 					s.functionLookup.MarkPodBusy(pod.PodName, pod.PodIP)
	// 					for i, addr := range addresses {
	// 						if addr.IP == newPodIP {
	// 							return i, nil
	// 						}
	// 					}
	// 				}
	// 			}
	// 		}
	// 	}
	// }
	// --------------- REMOVING THE SCALING LOGIC FOR NOW ---------------
	// 5. No idle pods found, return error
	log.Printf("No idle pods found for function %s in namespace %s, returning error", functionName, namespace)
	return -1, errors.New("no idle pods available for function " + functionName + " in namespace " + namespace)
}

// Helper to filter idle pods that are in the addresses list
func filterIdlePodsForAddresses(pods []PodStatus, addresses []corev1.EndpointAddress, max_inflight int) []PodStatus {
	addrSet := make(map[string]struct{}, len(addresses))
	for _, addr := range addresses {
		addrSet[addr.IP] = struct{}{}
	}
	idle := make([]PodStatus, 0, len(pods))
	for _, pod := range pods {
		if pod.Status == "idle" && pod.ActiveConnections < max_inflight {
			if _, ok := addrSet[pod.PodIP]; ok {
				idle = append(idle, pod)
			}
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

// Get function max_inflight from the deployment environment variables
func (s *IdleFirstSelector) getFunctionMaxInflight(functionName, namespace string) (int, error) {
	cacheKey := namespace + "/" + functionName

	// First, try cache
	if val, ok := s.maxInflightCache.Load(cacheKey); ok {
		return val.(int), nil
	}

	// Use singleflight to deduplicate concurrent requests
	val, err, _ := s.maxInflightGroup.Do(cacheKey, func() (interface{}, error) {
		deployments := s.clientset.AppsV1().Deployments(namespace)
		deployment, err := deployments.Get(context.TODO(), functionName, metav1.GetOptions{})
		if err != nil {
			return 0, err
		}
		functionStatus := AsFunctionStatus(*deployment)
		log.Printf("Function status: %+v", functionStatus)
		if functionStatus.EnvVars != nil {
			if val, ok := functionStatus.EnvVars["max_inflight"]; ok {
				maxInflight, err := strconv.Atoi(val)
				if err != nil {
					return 0, fmt.Errorf("invalid max_inflight value: %v", err)
				}
				// Store in cache
				s.maxInflightCache.Store(cacheKey, maxInflight)
				return maxInflight, nil
			}
		}
		return 0, errors.New("max_inflight not found in deployment environment variables")
	})
	if err != nil {
		return 0, err
	}
	return val.(int), nil
}

// scaleUpFunc scales the deployment for the function up by 1
// func (s *IdleFirstSelector) scaleUpFunc(functionName, namespace string) error {
// 	deployments := s.clientset.AppsV1().Deployments(namespace)
// 	deployment, err := deployments.Get(context.TODO(), functionName, metav1.GetOptions{})
// 	if err != nil {
// 		return err
// 	}
// 	desired := *deployment.Spec.Replicas + 1
// 	deployment.Spec.Replicas = &desired
// 	_, err = deployments.Update(context.TODO(), deployment, metav1.UpdateOptions{})
// 	return err
// }

// checkPodAvailable checks if a pod is available by making an HTTP request to its /_/ready endpoint.
// This respects the concurrency limits set in the of-watchdog.
func (s *IdleFirstSelector) checkPodAvailable(podIP string) bool {
	const watchdogPort = 8080
	const timeout = 500 * time.Millisecond

	if podIP == "" {
		return false
	}

	// url := fmt.Sprintf("http://%s:%d/_/ready", podIP, watchdogPort)
	// Use /_/health endpoint for availability check since not all functions may implement /_/ready
	url := fmt.Sprintf("http://%s:%d/_/health", podIP, watchdogPort)
	client := &http.Client{
		Timeout: timeout,
	}

	resp, err := client.Get(url)
	if err != nil {
		log.Printf("Error checking pod availability for %s: %v", podIP, err)
		return false
	}
	defer resp.Body.Close()

	// Only consider the pod available if it returns 200 OK
	return resp.StatusCode == http.StatusOK
}
