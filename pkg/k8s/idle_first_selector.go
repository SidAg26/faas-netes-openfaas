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

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/singleflight"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1" // for Deployment envVariable access
	"k8s.io/client-go/kubernetes"
)

// Queue depth metric (this was missing!)
var (
	queueDepthGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gateway_queue_depth",
			Help: "Current depth of function request queues",
		},
		[]string{"function_name", "namespace"},
	)
)

// SA - Metric registration for queue depth of the function
func init() {
	err := prometheus.Register(queueDepthGauge)
	if err != nil {
		if are, ok := err.(prometheus.AlreadyRegisteredError); ok {
			// Use existing metric if already registered
			if existingGauge, ok := are.ExistingCollector.(*prometheus.GaugeVec); ok {
				queueDepthGauge = existingGauge
				log.Printf("Queue depth metric already registered, using existing collector")
			}
		} else {
			log.Printf("Warning: Failed to register queue depth metric: %v", err)
		}
	} else {
		log.Printf("Queue depth metric registered successfully")
	}
}

// UpdateQueueDepth updates the queue depth metric
func UpdateQueueDepth(functionName, namespace string, depth int) {
	queueDepthGauge.WithLabelValues(functionName, namespace).Set(float64(depth))
}

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

	// SA - Adding requestQueue to manage queued requests
	requestQueue map[string]chan *QueuedRequest // functionName.namespace -> queue
	queueMux     sync.RWMutex
}

type QueuedRequest struct {
	Addresses    []corev1.EndpointAddress
	FunctionName string
	Namespace    string
	ResponseChan chan QueueResult
	StartTime    time.Time
	MaxWaitTime  time.Duration
	RetryCount   int // Add retry counter
	MaxRetries   int // Maximum retry attempts
}

type QueueResult struct {
	Index int
	Error error
}

func NewIdleFirstSelector(clientset *kubernetes.Clientset, podStatusCache *PodStatusCache, functionLookup *FunctionLookup) *IdleFirstSelector {
	return &IdleFirstSelector{
		clientset:      clientset,
		podStatusCache: podStatusCache,
		functionLookup: functionLookup,
		requestQueue:   make(map[string]chan *QueuedRequest),
	}
}

// Select returns the index of the pod to use, or -1 if none found.
// It implements the idle-first logic described in your prompt.
func (s *IdleFirstSelector) Select(
	addresses []corev1.EndpointAddress, requestID string, // SA - Add requestID for tracing
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
		log.Printf("[REQ:%s] Error getting max_inflight for function %s in namespace %s: %v", requestID, functionName, namespace, err)
	}

	// 1. Sync cache with endpoints (removes stale, adds new as idle)
	s.podStatusCache.PruneByAddresses(requestID, functionName, namespace, s.clientset, &addresses, max_inflight)

	// 2. Try to find an idle pod and use it
	if index, err := s.trySelectIdlePod(requestID, addresses, functionName, namespace, max_inflight); err == nil {
		return index, nil
	}
	// podStatuses := s.podStatusCache.GetByFunction(functionName, namespace)
	// idlePods := filterIdlePodsForAddresses(podStatuses, addresses, max_inflight)
	// tryCount := 0
	// for tryCount < 3 && len(idlePods) > 0 {
	// 	selected := idlePods[rand.Intn(len(idlePods))]
	// 	if s.checkPodAvailable(selected.PodIP) {
	// 		for i, addr := range addresses {
	// 			if addr.IP == selected.PodIP {
	// 				if s.podStatusCache.TryMarkPodBusy(selected.PodName, selected.PodIP) {
	// 					s.functionLookup.MarkPodBusy(selected.PodName, selected.PodIP)
	// 					// Found the index of the selected pod in the addresses list
	// 					log.Printf("Selected pod %s at index %d", selected.PodName, i)
	// 					return i, nil
	// 				} else {
	// 					// The pod was marked busy by another request, try again
	// 					s.podStatusCache.PruneByAddresses(functionName, namespace, s.clientset, &addresses, max_inflight)
	// 					idlePods = filterIdlePodsForAddresses(podStatuses, addresses, max_inflight)
	// 					continue
	// 				}

	// 			}
	// 		}
	// 	}
	// 	idlePods = removePodFromList(idlePods, selected.PodIP) // Pods that are unavailable not the busy ones
	// 	tryCount++
	// }
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
	log.Printf("[REQ:%s] No idle pods found for function %s in namespace %s, returning error", requestID, functionName, namespace)
	// return -1, errors.New("no idle pods available for function " + functionName + " in namespace " + namespace)
	// Instead of returning an error, we can queue the request
	return s.queueAndWaitForPod(requestID, addresses, functionName, namespace, max_inflight)
}

// SA - trySelectIdlePod attempts to select an idle pod from the provided addresses.
// It returns the index of the selected pod or an error if no idle pods are available.
func (s *IdleFirstSelector) trySelectIdlePod(requestID string, addresses []corev1.EndpointAddress, functionName, namespace string, max_inflight int) (int, error) {
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
						log.Printf("[REQ:%s][Select] Selected pod %s at index %d immediately", requestID, selected.PodName, i)
						// EXPLICIT CHECK: Ensure we never return -1 with nil error
						if i < 0 {
							return -1, errors.New("invalid pod index")
						}
						return i, nil
					} else {
						// Pod was marked busy by another request, refresh and try again
						s.podStatusCache.PruneByAddresses(requestID, functionName, namespace, s.clientset, &addresses, max_inflight)
						podStatuses = s.podStatusCache.GetByFunction(functionName, namespace)
						idlePods = filterIdlePodsForAddresses(podStatuses, addresses, max_inflight)
						continue
					}
				}
			}
		}
		idlePods = removePodFromList(idlePods, selected.PodIP)
		tryCount++
	}

	return -1, errors.New("no idle pods available")
}

func (s *IdleFirstSelector) queueAndWaitForPod(requestID string, addresses []corev1.EndpointAddress, functionName, namespace string, max_inflight int) (int, error) {
	key := functionName + "." + namespace

	// Create or get the queue for this function
	s.queueMux.Lock()
	queue, exists := s.requestQueue[key]
	if !exists {
		queue = make(chan *QueuedRequest, 10) // Buffer of 10 requests per function
		s.requestQueue[key] = queue
		go s.processQueue(requestID, key, functionName, namespace) // Start queue processor
	}

	// Update queue depth metric
	currentDepth := len(queue)
	UpdateQueueDepth(functionName, namespace, currentDepth)

	s.queueMux.Unlock()

	// Create queued request with retry settings
	queuedRequest := &QueuedRequest{
		Addresses:    addresses,
		FunctionName: functionName,
		Namespace:    namespace,
		ResponseChan: make(chan QueueResult, 1),
		StartTime:    time.Now(),
		MaxWaitTime:  100 * time.Millisecond,
		RetryCount:   0,  // Start with 0 retries
		MaxRetries:   10, // Allow up to 10 retries (10ms * 10 = 100ms max)
	}

	// Try to enqueue
	select {
	case queue <- queuedRequest:
		// Wait for result from queue processor
		select {
		case result := <-queuedRequest.ResponseChan:
			if result.Error != nil {
				log.Printf("[REQ:%s] [Queue] Request for %s.%s failed after %v: %v",
					requestID, functionName, namespace, time.Since(queuedRequest.StartTime), result.Error)
				return -1, result.Error
			}
			log.Printf("[REQ:%s] [Queue] Request for %s.%s succeeded after %v, pod index: %d",
				requestID, functionName, namespace, time.Since(queuedRequest.StartTime), result.Index)
			return result.Index, nil

		case <-time.After(150 * time.Millisecond): // 50ms buffer beyond the 100ms wait
			return -1, fmt.Errorf("[REQ:%s] request timeout after 150ms waiting for idle pod", requestID)
		}

	default:
		// Queue is full
		return -1, fmt.Errorf("[REQ:%s] request queue full for function %s.%s", requestID, functionName, namespace)
	}
}

// SA - Process the queue for a specific function
func (s *IdleFirstSelector) processQueue(requestID, key, functionName, namespace string) {
	s.queueMux.RLock()
	queue := s.requestQueue[key]
	s.queueMux.RUnlock()

	ticker := time.NewTicker(10 * time.Millisecond) // Check every 10ms
	defer ticker.Stop()

	log.Printf("[REQ:%s] [Queue] Started queue processor for %s.%s", requestID, functionName, namespace)

	for {
		select {
		case queuedRequest := <-queue:
			log.Printf("[REQ:%s] [Queue] Processing request for %s.%s (attempt %d/%d, elapsed: %v)",
				requestID, functionName, namespace, queuedRequest.RetryCount+1, queuedRequest.MaxRetries+1,
				time.Since(queuedRequest.StartTime))

			// Check if request has timed out
			elapsed := time.Since(queuedRequest.StartTime)
			if elapsed > queuedRequest.MaxWaitTime {
				log.Printf("[REQ:%s] [Queue] Request timed out after %v for %s.%s", requestID, elapsed, functionName, namespace)
				queuedRequest.ResponseChan <- QueueResult{
					Index: -1,
					Error: fmt.Errorf("[REQ:%s] request timeout after %v", requestID, elapsed),
				}
				continue
			}

			// Try to get max_inflight again (in case it changed)
			max_inflight, err := s.getFunctionMaxInflight(functionName, namespace)
			if err != nil {
				max_inflight = math.MaxInt32
			}

			// Refresh pod status and try to select an idle pod
			s.podStatusCache.PruneByAddresses(requestID, functionName, namespace, s.clientset, &queuedRequest.Addresses, max_inflight)

			if index, err := s.trySelectIdlePod(requestID, queuedRequest.Addresses, functionName, namespace, max_inflight); err == nil {
				// SUCCESS: Found an idle pod
				if index >= 0 && index < len(queuedRequest.Addresses) {
					log.Printf("[REQ:%s] [Queue] SUCCESS: Found idle pod at index %d for %s.%s after %v (attempt %d)",
						requestID, index, functionName, namespace, elapsed, queuedRequest.RetryCount+1)
					queuedRequest.ResponseChan <- QueueResult{
						Index: index,
						Error: nil,
					}
				} else {
					log.Printf("[REQ:%s] [Queue] ERROR: Invalid index %d returned for %s.%s", requestID, index, functionName, namespace)
					queuedRequest.ResponseChan <- QueueResult{
						Index: -1,
						Error: fmt.Errorf("[REQ:%s] invalid pod index returned: %d", requestID, index),
					}
				}
			} else {
				// Still no idle pods - check if we should retry or give up
				if queuedRequest.RetryCount < queuedRequest.MaxRetries && elapsed < queuedRequest.MaxWaitTime {
					// Increment retry count and requeue
					queuedRequest.RetryCount++
					select {
					case queue <- queuedRequest:
						log.Printf("[REQ:%s] [Queue] Requeued request for %s.%s (attempt %d/%d, elapsed: %v)",
							requestID, functionName, namespace, queuedRequest.RetryCount, queuedRequest.MaxRetries+1, elapsed)
					default:
						// Queue full, fail the request
						log.Printf("[REQ:%s] [Queue] Queue full when trying to requeue %s.%s after %d attempts",
							requestID, functionName, namespace, queuedRequest.RetryCount)
						queuedRequest.ResponseChan <- QueueResult{
							Index: -1,
							Error: fmt.Errorf("[REQ:%s] queue full, cannot requeue request after %d attempts", requestID, queuedRequest.RetryCount),
						}
					}
				} else {
					// Max retries exceeded or timeout
					if queuedRequest.RetryCount >= queuedRequest.MaxRetries {
						log.Printf("[REQ:%s] [Queue] Max retries (%d) exceeded for %s.%s after %v",
							requestID, queuedRequest.MaxRetries, functionName, namespace, elapsed)
						queuedRequest.ResponseChan <- QueueResult{
							Index: -1,
							Error: fmt.Errorf("[REQ:%s] max retries (%d) exceeded, no idle pods available", requestID, queuedRequest.MaxRetries),
						}
					} else {
						log.Printf("[REQ:%s] [Queue] Timeout reached for %s.%s after %v (attempt %d)",
							requestID, functionName, namespace, elapsed, queuedRequest.RetryCount+1)
						queuedRequest.ResponseChan <- QueueResult{
							Index: -1,
							Error: fmt.Errorf("[REQ:%s] no idle pods became available within %v", requestID, queuedRequest.MaxWaitTime),
						}
					}
				}
			}

			//  case <-ticker.C:
			// 	// Periodic tick - could be used for metrics or cleanup if needed
			// 	continue
		}
	}
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
