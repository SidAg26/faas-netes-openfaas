package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/openfaas/faas-netes/pkg/k8s"
)

func MakePodsStatusFetchHandler(lookup *k8s.FunctionLookup) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		functionName := r.URL.Query().Get("functionName")
		namespace := r.URL.Query().Get("namespace")

		if functionName == "" || namespace == "" {
			http.Error(w, "functionName and namespace are required", http.StatusBadRequest)
			return
		}

		statuses, err := lookup.GetPodStatusByFunction(functionName, namespace)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if len(statuses) == 0 {
			http.Error(w, "no pods found", http.StatusNotFound)
			return
		}

		responseBytes, err := json.Marshal(statuses)
		if err != nil {
			http.Error(w, "failed to encode response", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(responseBytes)

	}
}
