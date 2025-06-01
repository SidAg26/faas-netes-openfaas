// License: OpenFaaS Community Edition (CE) EULA
// Copyright (c) 2017,2019-2024 OpenFaaS Author(s)

// Copyright (c) Alex Ellis 2017. All rights reserved\.
// Copyright 2020 OpenFaaS Author(s)

package k8s

import (
	"fmt"
	"math/rand"
	"net/url"
	"strings"
	"sync"
	// SA - Add the logging package
	"log"

	corelister "k8s.io/client-go/listers/core/v1"
)

// watchdogPort for the OpenFaaS function watchdog
const watchdogPort = 8080

func NewFunctionLookup(ns string, lister corelister.EndpointsLister) *FunctionLookup {
	return &FunctionLookup{
		DefaultNamespace: ns,
		EndpointLister:   lister,
		Listers:          map[string]corelister.EndpointsNamespaceLister{},
		lock:             sync.RWMutex{},
		// SA - Add the Round-Robin module
		rrSelector: NewRoundRobinSelector(), // Initialize the Round-Robin selector
	}
}

type FunctionLookup struct {
	DefaultNamespace string
	EndpointLister   corelister.EndpointsLister
	Listers          map[string]corelister.EndpointsNamespaceLister

	lock sync.RWMutex

	// SA - Add the Round-Robin module
	rrSelector *RoundRobinSelector // Round-Robin selector for function endpoints

	// // SA - Add the Round-Robin strategy last index tracker
	// // for each function-namespace combination.
	// rrLock sync.RWMutex // lock for rrLastSelected
	// rrLastSelected map[string]int // key: functionName.namespace, value: last index
}

func (f *FunctionLookup) GetLister(ns string) corelister.EndpointsNamespaceLister {
	f.lock.RLock()
	defer f.lock.RUnlock()
	return f.Listers[ns]
}

func (f *FunctionLookup) SetLister(ns string, lister corelister.EndpointsNamespaceLister) {
	f.lock.Lock()
	defer f.lock.Unlock()
	f.Listers[ns] = lister
}

func getNamespace(name, defaultNamespace string) string {
	namespace := defaultNamespace
	if strings.Contains(name, ".") {
		namespace = name[strings.LastIndexAny(name, ".")+1:]
	}
	return namespace
}

// This method resolves a function name to a URL.
// It extracts the function name and namespace from the provided name,
// verifies the namespace, and retrieves the corresponding service endpoints.
// If the service has no subsets or addresses, it returns an error.
// If successful, it randomly selects an address from the service's endpoints
// and constructs a URL pointing to the function's watchdog port (8080).
func (l *FunctionLookup) Resolve(name string) (url.URL, error) {
	functionName := name
	namespace := getNamespace(name, l.DefaultNamespace)
	if err := l.verifyNamespace(namespace); err != nil {
		return url.URL{}, err
	}

	if strings.Contains(name, ".") {
		functionName = strings.TrimSuffix(name, "."+namespace)
	}

	nsEndpointLister := l.GetLister(namespace)

	if nsEndpointLister == nil {
		l.SetLister(namespace, l.EndpointLister.Endpoints(namespace))

		nsEndpointLister = l.GetLister(namespace)
	}

	svc, err := nsEndpointLister.Get(functionName)
	if err != nil {
		return url.URL{}, fmt.Errorf("error listing \"%s.%s\": %s", functionName, namespace, err.Error())
	}

	if len(svc.Subsets) == 0 {
		return url.URL{}, fmt.Errorf("no subsets available for \"%s.%s\"", functionName, namespace)
	}

	// This is where the faas-netes controller
	// creates the service with a single subset i.e.
	// it looks for the available pods from the service/deployment
	// and forwards the request to one of them.
	// However, if there are no pods available, the faas-provider 
	// interface handles the creation/scaling of the functions
	// when the request arrives.
	all := len(svc.Subsets[0].Addresses)
	if len(svc.Subsets[0].Addresses) == 0 {
		return url.URL{}, fmt.Errorf("no addresses in subset for \"%s.%s\"", functionName, namespace)
	}

	target := rand.Intn(all)
	// SA - ToDo: 1. Round-Robin selection
	key := functionName + "." + namespace
	target = l.rrSelector.Next(key, all)
	log.Printf("Selected target index %d for function %s in namespace %s", target, functionName, namespace)

	// // SA - ToDo: 
	// // Instead of randomly selecting an address,
	// // what other strategies could be used?
	// // 1. Round-robin selection
	// // 2. Least connections
	// // 3. Weighted distribution based on previous response times

	// // SA - 1. Round-robin selection
	// key := functionName + "." + namespace
	// l.rrLock.Lock()
	// if l.rrLastSelected == nil {
	// 	l.rrLastSelected = make(map[string]int) // Initialize the map if it doesn't exist
	// }
	// last := l.rrLastSelected[key]

	// // SA - ensure the last index is within the range of available addresses
	// if last >= all  || last < 0 {
	// 	// If last is out of bounds, reset it to 0
	// 	// This can happen if the service was updated or restarted
	// 	// and the last selected index is no longer valid.
	// 	// This ensures that we always start from the first address
	// 	last = 0
	// 	l.rrLastSelected[key] = last
	// }

	// next := (last + 1) % all
	// l.rrLastSelected[key] = next
	// l.rrLock.Unlock()
	// target = next
	// -------------------------------

	serviceIP := svc.Subsets[0].Addresses[target].IP

	urlStr := fmt.Sprintf("http://%s:%d", serviceIP, watchdogPort)

	urlRes, err := url.Parse(urlStr)
	if err != nil {
		return url.URL{}, err
	}

	return *urlRes, nil
}

func (l *FunctionLookup) verifyNamespace(name string) error {
	if name != "kube-system" {
		return nil
	}
	// ToDo use global namepace parse and validation
	return fmt.Errorf("namespace not allowed")
}
