package k8s

import (
	"context"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ScalingRequest represents a request to scale a function
type ScalingRequest struct {
	FunctionName string
	Namespace    string
	Timestamp    time.Time
}

// ScalingQueue manages scaling requests to avoid frequent deployment updates
type ScalingQueue struct {
	clientset       *kubernetes.Clientset
	mutex           sync.Mutex
	pendingRequests map[string]ScalingRequest
	processing      bool
	interval        time.Duration
	stopCh          chan struct{}
}

// NewScalingQueue creates a new scaling queue
func NewScalingQueue(clientset *kubernetes.Clientset) *ScalingQueue {
	sq := &ScalingQueue{
		clientset:       clientset,
		pendingRequests: make(map[string]ScalingRequest),
		interval:        2 * time.Second, // Process scaling requests every 2 seconds
		stopCh:          make(chan struct{}),
	}
	
	go sq.processQueue()
	return sq
}

// Stop stops the scaling queue processor
func (sq *ScalingQueue) Stop() {
	close(sq.stopCh)
}

// QueueScalingRequest adds a scaling request to the queue
func (sq *ScalingQueue) QueueScalingRequest(functionName, namespace string) {
	key := namespace + "/" + functionName
	
	sq.mutex.Lock()
	defer sq.mutex.Unlock()
	
	// Only add if not already in the queue
	if _, exists := sq.pendingRequests[key]; !exists {
		sq.pendingRequests[key] = ScalingRequest{
			FunctionName: functionName,
			Namespace:    namespace,
			Timestamp:    time.Now(),
		}
	}
}

// processQueue processes the scaling queue at regular intervals
func (sq *ScalingQueue) processQueue() {
	ticker := time.NewTicker(sq.interval)
	defer ticker.Stop()
	
	for {
		select {
		case <-ticker.C:
			sq.processRequests()
		case <-sq.stopCh:
			return
		}
	}
}

// processRequests processes all pending scaling requests
func (sq *ScalingQueue) processRequests() {
	sq.mutex.Lock()
	
	// If no requests or already processing, return
	if len(sq.pendingRequests) == 0 || sq.processing {
		sq.mutex.Unlock()
		return
	}
	
	// Copy requests and mark as processing
	requests := make([]ScalingRequest, 0, len(sq.pendingRequests))
	for _, req := range sq.pendingRequests {
		requests = append(requests, req)
	}
	sq.pendingRequests = make(map[string]ScalingRequest)
	sq.processing = true
	
	sq.mutex.Unlock()
	
	// Process requests outside the lock
	for _, req := range requests {
		sq.scaleFunction(req.FunctionName, req.Namespace)
	}
	
	// Mark processing as complete
	sq.mutex.Lock()
	sq.processing = false
	sq.mutex.Unlock()
}

// scaleFunction scales a function by updating its deployment
func (sq *ScalingQueue) scaleFunction(functionName, namespace string) {
	deployments := sq.clientset.AppsV1().Deployments(namespace)
	
	// Use retry with exponential backoff
	maxRetries := 5
	for i := 0; i < maxRetries; i++ {
		// Get the latest deployment
		deployment, err := deployments.Get(context.TODO(), functionName, metav1.GetOptions{})
		if err != nil {
			// Log error and continue to next request
			return
		}
		
		// Increase replicas
		desired := *deployment.Spec.Replicas + 1
		deployment.Spec.Replicas = &desired
		
		// Update the deployment
		_, err = deployments.Update(context.TODO(), deployment, metav1.UpdateOptions{})
		if err == nil {
			// Success
			return
		}
		
		// If conflict error, retry after a short delay
		time.Sleep(time.Duration(50*(i+1)) * time.Millisecond)
	}
}

// GetCurrentReplicas gets the current replica count for a function
func (sq *ScalingQueue) GetCurrentReplicas(functionName, namespace string) (int32, error) {
	deployment, err := sq.clientset.AppsV1().Deployments(namespace).Get(
		context.TODO(), 
		functionName, 
		metav1.GetOptions{},
	)
	if err != nil {
		return 0, err
	}
	return *deployment.Spec.Replicas, nil
}

// CreateStandbyPod creates a standby pod for a function without updating the deployment
func (sq *ScalingQueue) CreateStandbyPod(functionName, namespace string) error {
	// This would be implemented if we wanted to create pods directly
	// For now, we're just using deployment scaling
	return nil
}