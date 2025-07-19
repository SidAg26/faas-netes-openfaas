// SA - pod_status.go
// This file provides a thread-safe cache for pod statuses in a Kubernetes environment.
// It allows setting, getting, and retrieving pod statuses by function name or all pods.
// It also includes the pod's status, IP address, function name, and namespace.

package k8s

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"                   // SA - Importing the corev1 package for Kubernetes core API interactions
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1" // SA - Importing the metav1 package for Kubernetes API interactions
	"k8s.io/client-go/kubernetes"                 // SA - Importing the Kubernetes client package
)

// PodStatus represents the status of a pod
type PodStatus struct {
	PodName           string    // The name of the pod
	Status            string    // "busy" or "idle"
	Timestamp         time.Time // When the status was last updated
	PodIP             string    // The IP address of the pod
	Function          string    // The function name this pod belongs to
	Namespace         string    // The namespace of the pod
	ActiveConnections int       // The number of active connections to the pod - for "max_inflight" support
	MaxInflight       *int      // Optional: Maximum number of inflight requests for this pod
	PodUID            string    // Optional: Unique identifier for the pod, if available
}

// PodStatusCache provides a thread-safe cache for pod status
type PodStatusCache struct {
	cache     sync.Map              // Maps podName-IP -> PodStatus
	podLocks  sync.Map              // Maps podName-IP -> *sync.Mutex for per-pod locking
	clientset *kubernetes.Clientset // Optional: Kubernetes client for interacting with the API
}

// NewPodStatusCache creates a new pod status cache
func NewPodStatusCache() *PodStatusCache {
	return &PodStatusCache{
		cache: sync.Map{},
	}
}

// createKey creates a composite key from podName and podIP
func (p *PodStatusCache) createKey(podName, podIP string) string {
	return podName + "-" + podIP
}

func (p *PodStatusCache) TryMarkPodBusy(podName, podIP string) bool {
	key := p.createKey(podName, podIP)
	// Lock per-pod (using a sync.Map of mutexes)
	// Generates the key.
	lockIface, _ := p.podLocks.LoadOrStore(key, &sync.Mutex{})
	// Retrieves or creates a mutex for this pod.
	lock := lockIface.(*sync.Mutex)
	// Locks the mutex to ensure atomicity for this pod’s status update.
	lock.Lock()
	defer lock.Unlock()

	status, ok := p.cache.Load(key)
	if !ok {
		log.Printf("Pod %s not found in cache", key)
		return false
	}
	podStatus := status.(PodStatus)
	if podStatus.Status == "busy" || (podStatus.MaxInflight != nil && podStatus.ActiveConnections >= *podStatus.MaxInflight) {
		return false // Already busy or at limit
	}
	// podStatus.ActiveConnections++
	// if podStatus.MaxInflight != nil && podStatus.ActiveConnections >= *podStatus.MaxInflight {
	// 	podStatus.Status = "busy"
	// }
	// p.cache.Store(key, podStatus)
	return true
}

// Set updates the status of a pod
func (p *PodStatusCache) Set(podName, status, podIP, function, namespace string, maxInflight *int) {
	key := p.createKey(podName, podIP)

	// If the "status" is "busy", we update the active connections count by +1
	// If the "status" is "idle", we update the active connections count by -1
	// If the status is neither, we keep the current count or default to 0
	var (
		activeConnections int
		finalStatus       string
	)

	// Generates the key.
	lockIface, _ := p.podLocks.LoadOrStore(key, &sync.Mutex{})
	// Retrieves or creates a mutex for this pod.
	lock := lockIface.(*sync.Mutex)
	// Locks the mutex to ensure atomicity for this pod’s status update.
	lock.Lock()
	defer lock.Unlock()

	// If value is found in the cache, we update it
	if value, exists := p.cache.Load(key); exists {
		current := value.(PodStatus)
		if current.MaxInflight == nil {
			current.MaxInflight = maxInflight
		}
		if status == "busy" {
			activeConnections = current.ActiveConnections + 1
			if maxInflight != nil && activeConnections >= *current.MaxInflight {
				finalStatus = "busy"
			} else {
				finalStatus = "idle" // If not at max inflight, we consider it idle
			}
		} else if status == "idle" {
			activeConnections = max(current.ActiveConnections-1, 0)
			finalStatus = "idle"
		} else if status == "reset" {
			activeConnections = 0
			finalStatus = "idle" // Resetting to idle
		} else {
			activeConnections = current.ActiveConnections
			finalStatus = status
		}
	}
	// If value is not found in the cache, we create a new entry
	if _, exists := p.cache.Load(key); !exists {
		activeConnections = 0
		if activeConnections >= *maxInflight {
			finalStatus = "busy"
		} else {
			finalStatus = "idle" // If not at max inflight, we consider it idle
		}
	}
	// Get current pod UID if clientset is available
	var podUID string
	if p.clientset != nil {
		if pod, err := p.clientset.CoreV1().Pods(namespace).Get(context.TODO(), podName, metav1.GetOptions{}); err == nil {
			podUID = string(pod.UID)
		}
	}

	log.Printf("Setting pod status: %s, IP: %s, Function: %s, Namespace: %s, Status: %s, ActiveConnections: %d",
		podName, podIP, function, namespace, finalStatus, activeConnections)
	p.cache.Store(key, PodStatus{
		Status:            finalStatus,
		Timestamp:         time.Now(),
		PodIP:             podIP,
		Function:          function,
		Namespace:         namespace,
		PodName:           podName,
		ActiveConnections: activeConnections,
		MaxInflight:       maxInflight,
		PodUID:            podUID, // Optional: You can set this if you have the pod UID available
	})
}

// Get retrieves the status of a pod by podName and podIP
func (p *PodStatusCache) Get(podName, podIP string) (PodStatus, bool) {
	key := p.createKey(podName, podIP)
	value, exists := p.cache.Load(key)
	if !exists {
		return PodStatus{}, false
	}
	return value.(PodStatus), true
}

// GetByPodName retrieves the status of a pod by podName only
func (p *PodStatusCache) GetByPodName(podName string) []PodStatus {
	result := []PodStatus{}

	p.cache.Range(func(key, value interface{}) bool {
		keyStr := key.(string)
		if len(keyStr) > len(podName) && keyStr[:len(podName)] == podName && keyStr[len(podName)] == '-' {
			result = append(result, value.(PodStatus))
		}
		return true
	})

	return result
}

// Add a function-level lock mechanism using the existing podLocks
func (p *PodStatusCache) getFunctionLock(function, namespace string) *sync.Mutex {
	key := "function-" + function + "-" + namespace
	lockIface, _ := p.podLocks.LoadOrStore(key, &sync.Mutex{})
	return lockIface.(*sync.Mutex)
}

// GetByFunction returns all pods for a specific function
func (p *PodStatusCache) GetByFunction(function, namespace string) []PodStatus {
	lock := p.getFunctionLock(function, namespace)
	lock.Lock()
	defer lock.Unlock()

	var result []PodStatus                                             // making sure to use a copy of the slice
	addresses := refreshAddresses(function, namespace, p.clientset)    // Refresh addresses before filtering
	addrSet := make(map[string]corev1.EndpointAddress, len(addresses)) // Use a map to track unique addresses
	for _, addr := range addresses {
		addrSet[addr.IP] = addr
	}
	// 1. Remove stale entries
	p.cache.Range(func(key, value interface{}) bool {
		pod := value.(PodStatus)
		if pod.Function == function && pod.Namespace == namespace {
			if _, ok := addrSet[pod.PodIP]; !ok {
				p.cache.Delete(key)
			}
		}
		return true
	})
	var max_inflight int
	// 2. Add new endpoints as idle if not present AND check for pod restarts
	for ip, addr := range addrSet {
		found := false
		p.cache.Range(func(key, value interface{}) bool {
			pod := value.(PodStatus)
			if pod.Function == function && pod.Namespace == namespace && pod.PodIP == ip {
				found = true
				log.Printf("[REQ:%s] Checking pod UID for %s in namespace %s", "not capturing", pod.PodName, namespace)
				// Check if pod restarted by comparing UIDs
				if p.clientset != nil && addr.TargetRef != nil {
					currentPod, err := p.clientset.CoreV1().Pods(namespace).Get(context.TODO(), addr.TargetRef.Name, metav1.GetOptions{})
					if err == nil && string(currentPod.UID) != pod.PodUID {
						log.Printf("[REQ:%s] [UID-RESET] Pod %s UID changed: cached=%s, current=%s",
							"not capturing", pod.PodName, pod.PodUID, string(currentPod.UID))
						p.Set(pod.PodName, "reset", ip, function, namespace, pod.MaxInflight)
						p.setPodUID(pod.PodName, ip, string(currentPod.UID)) // Update the UID in the cache
					} else if err != nil {
						log.Printf("[REQ:%s] [UID-ERROR] Failed to get current pod %s: %v",
							"not capturing", pod.PodName, err)
					}
				} else {
					log.Printf("[REQ:%s] [UID-INFO] No TargetRef for address %s, using cached UID %s",
						"not capturing", ip, pod.PodUID)
				}

				return false // stop searching
			}
			return true
		})
		if !found {
			// Use addr.TargetRef.Name if available, else use IP as PodName
			podName := ip
			var podUID string
			if addr.TargetRef != nil && addr.TargetRef.Name != "" {
				podName = addr.TargetRef.Name
				if pod, err := p.clientset.CoreV1().Pods(namespace).Get(context.TODO(), podName, metav1.GetOptions{}); err == nil {
					podUID = string(pod.UID)

					for _, container := range pod.Spec.Containers {
						for _, env := range container.Env {
							if env.Name == "max_inflight" {
								if value, err := strconv.Atoi(env.Value); err == nil {
									max_inflight = value
									log.Printf("[REQ:%s] Found max_inflight for pod %s: %d", "not capturing", podName, max_inflight)

								} else {
									log.Printf("[REQ:%s] Error parsing max_inflight for pod %s: %v", "not capturing", podName, err)
								}
								break // No need to check other containers
							}
						}
						if max_inflight != 0 {
							break // Exit the loop if we found max_inflight
						}

					}
				}
				p.Set(podName, "idle", ip, function, namespace, &max_inflight)
				p.setPodUID(podName, ip, podUID) // Set the UID in the cache if available
			}
		}
		p.cache.Range(func(key, value interface{}) bool {
			status := value.(PodStatus)
			if status.Function == function && status.Namespace == namespace && checkPodAvailable(status.PodIP) {
				result = append(result, status)
			}
			return true
		})
	}

	return result // This is a copy of the slice, not a reference for safe use
}

func checkPodAvailable(podIP string) bool {
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

// GetAll returns all pod statuses
func (p *PodStatusCache) GetAll() map[string]PodStatus {
	result := make(map[string]PodStatus)

	p.cache.Range(func(key, value interface{}) bool {
		compositeKey := key.(string)
		status := value.(PodStatus)
		result[compositeKey] = status
		return true
	})

	return result
}

// GetByPodIP retrieves pod status by IP address
func (p *PodStatusCache) GetByPodIP(podIP string) []PodStatus {
	result := []PodStatus{}

	p.cache.Range(func(key, value interface{}) bool {
		status := value.(PodStatus)
		if status.PodIP == podIP {
			result = append(result, status)
		}
		return true
	})

	return result
}

// // SA - This function retrieves the global lock for the cache.
// // It ensures that only one operation can modify the cache at a time.
// func (p *PodStatusCache) getGlobalLock() *sync.Mutex {
// 	key := "global-cache-lock"
// 	lockIface, _ := p.podLocks.LoadOrStore(key, &sync.Mutex{})
// 	return lockIface.(*sync.Mutex)
// }

// Helper to refresh addresses from Kubernetes Endpoints
func refreshAddresses(functionName, namespace string, clientset *kubernetes.Clientset) []corev1.EndpointAddress {
	endpoints, err := clientset.CoreV1().Endpoints(namespace).Get(context.TODO(), functionName, metav1.GetOptions{})
	if err != nil || len(endpoints.Subsets) == 0 {
		return nil
	}
	var all []corev1.EndpointAddress
	for _, subset := range endpoints.Subsets {
		all = append(all, subset.Addresses...)
	}
	log.Printf("Refreshed addresses for function %s in namespace %s: %d addresses found", functionName, namespace, len(all))
	return all
}

// Prune the cache by removing old entries and keeping only the most recent status for each pod
func (c *PodStatusCache) PruneByAddresses(requestID, function, namespace string,
	clientset *kubernetes.Clientset, addresses *[]corev1.EndpointAddress,
	max_inflight int) {
	lock := c.getFunctionLock(function, namespace)
	lock.Lock()
	defer lock.Unlock()

	// 0. Refresh addresses from Kubernetes Endpoints
	validAddresses := refreshAddresses(function, namespace, clientset)
	if validAddresses == nil {
		log.Printf("[REQ:%s] Failed to refresh addresses for function %s in namespace %s", requestID, function, namespace)
	}
	if addresses == nil || len(*addresses) == 0 {
		// If no addresses are provided or the refresh failed, use the refreshed addresses
		addresses = &validAddresses
	}
	addrSet := make(map[string]corev1.EndpointAddress, len(*addresses))
	for _, addr := range *addresses {
		addrSet[addr.IP] = addr
	}
	// 1. Remove stale entries
	c.cache.Range(func(key, value interface{}) bool {
		pod := value.(PodStatus)
		if pod.Function == function && pod.Namespace == namespace {
			if _, ok := addrSet[pod.PodIP]; !ok {
				c.cache.Delete(key)
			}
		}
		return true
	})
	// 2. Add new endpoints as idle if not present AND check for pod restarts
	for ip, addr := range addrSet {
		found := false
		c.cache.Range(func(key, value interface{}) bool {
			pod := value.(PodStatus)
			if pod.Function == function && pod.Namespace == namespace && pod.PodIP == ip {
				found = true

				log.Printf("[REQ:%s] Checking pod UID for %s in namespace %s", requestID, pod.PodName, namespace)
				// Check if pod restarted by comparing UIDs
				if clientset != nil && addr.TargetRef != nil {
					currentPod, err := clientset.CoreV1().Pods(namespace).Get(context.TODO(), addr.TargetRef.Name, metav1.GetOptions{})
					if err == nil && string(currentPod.UID) != pod.PodUID {
						log.Printf("[REQ:%s] [UID-RESET] Pod %s UID changed: cached=%s, current=%s",
							requestID, pod.PodName, pod.PodUID, string(currentPod.UID))
						c.Set(pod.PodName, "reset", ip, function, namespace, pod.MaxInflight)
						c.setPodUID(pod.PodName, ip, string(currentPod.UID)) // Update the UID in the cache
					} else if err != nil {
						log.Printf("[REQ:%s] [UID-ERROR] Failed to get current pod %s: %v",
							requestID, pod.PodName, err)
					} else {
						// reset pods that have been busy for too long
						if pod.Status == "busy" && time.Since(pod.Timestamp) > 15*time.Minute {
							log.Printf("[REQ:%s] [UID-RESET] Pod %s is busy for too long, resetting...", requestID, pod.PodName)
							c.Set(pod.PodName, "reset", ip, function, namespace, pod.MaxInflight)
						}
					}
				} else {
					log.Printf("[REQ:%s] [UID-INFO] No TargetRef for address %s, using cached UID %s",
						requestID, ip, pod.PodUID)
				}

				return false // stop searching
			}
			return true
		})
		if !found {
			// Use addr.TargetRef.Name if available, else use IP as PodName
			podName := ip
			var podUID string
			if addr.TargetRef != nil && addr.TargetRef.Name != "" {
				podName = addr.TargetRef.Name
				if pod, err := clientset.CoreV1().Pods(namespace).Get(context.TODO(), podName, metav1.GetOptions{}); err == nil {
					podUID = string(pod.UID)
				}
			}
			c.Set(podName, "idle", ip, function, namespace, &max_inflight)
			c.setPodUID(podName, ip, podUID) // Set the UID in the cache if available
		}
	}
}

func (p *PodStatusCache) setPodUID(podName, podIP, podUID string) {
	key := p.createKey(podName, podIP)

	// Lock per-pod (using a sync.Map of mutexes)
	lockIface, _ := p.podLocks.LoadOrStore(key, &sync.Mutex{})
	lock := lockIface.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()

	if value, exists := p.cache.Load(key); exists {
		current := value.(PodStatus)
		current.PodUID = podUID
		p.cache.Store(key, current)
	} else {
		log.Printf("Pod %s not found in cache to set UID", key)
	}
}
