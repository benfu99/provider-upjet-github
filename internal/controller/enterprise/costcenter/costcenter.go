/*
Copyright 2022 Upbound Inc.
*/

package costcenter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	xpv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	"github.com/crossplane/crossplane-runtime/pkg/event"
	"github.com/crossplane/crossplane-runtime/pkg/logging"
	"github.com/crossplane/crossplane-runtime/pkg/resource"
	"github.com/crossplane/upjet/pkg/controller"

	"github.com/crossplane-contrib/provider-upjet-github/apis/enterprise/v1alpha1"
	apisv1beta1 "github.com/crossplane-contrib/provider-upjet-github/apis/v1beta1"
)

const (
	errNotCostCenter    = "managed resource is not a CostCenter custom resource"
	errTrackPCUsage     = "cannot track ProviderConfig usage"
	errGetPC            = "cannot get ProviderConfig"
	errGetCreds         = "cannot get credentials"
	errNewClient        = "cannot create new GitHub client"
	errCreateCostCenter = "cannot create cost center"
	errGetCostCenter    = "cannot get cost center"
	errUpdateCostCenter = "cannot update cost center"
	errDeleteCostCenter = "cannot delete cost center"
)

// Setup adds a controller that reconciles CostCenter managed resources.
func Setup(mgr ctrl.Manager, o controller.Options) error {
	name := "costcenter-direct"

	o.Logger.Info("Setting up CostCenter controller", "name", name)

	// Use DirectCostCenterReconciler which has working deletion functionality
	reconciler := &DirectCostCenterReconciler{
		Client:       mgr.GetClient(),
		Scheme:       mgr.GetScheme(),
		Logger:       o.Logger.WithValues("controller", name),
		newServiceFn: newGitHubService,
		recorder:     event.NewAPIRecorder(mgr.GetEventRecorderFor(name)),
	}

	err := ctrl.NewControllerManagedBy(mgr).
		Named(name).
		WithOptions(o.ForControllerRuntime()).
		For(&v1alpha1.CostCenter{}).
		Complete(reconciler)

	if err != nil {
		return err
	}

	o.Logger.Info("Successfully setup CostCenter controller", "name", name)
	return nil
}

// CostCenter represents a GitHub cost center
type CostCenter struct {
	ID        *string    `json:"id,omitempty"`
	Name      *string    `json:"name,omitempty"`
	State     *string    `json:"state,omitempty"`
	Resources []Resource `json:"resources,omitempty"`
}

// Resource represents a resource associated with a cost center
type Resource struct {
	Type *string `json:"type,omitempty"`
	Name *string `json:"name,omitempty"`
}

// GitHubService defines the interface for GitHub cost center operations
type GitHubService interface {
	CreateCostCenter(ctx context.Context, enterprise, name string) (*CostCenter, error)
	GetCostCenter(ctx context.Context, enterprise, costCenterID string) (*CostCenter, error)
	ListCostCenters(ctx context.Context, enterprise string) ([]CostCenter, error)
	UpdateCostCenter(ctx context.Context, enterprise, costCenterID, name string) (*CostCenter, error)
	DeleteCostCenter(ctx context.Context, enterprise, costCenterID string) error
}

// gitHubService implements GitHubService using raw HTTP requests
type gitHubService struct {
	token   string
	baseURL string
	client  *http.Client
}

func newGitHubService(ctx context.Context, token string, baseURL string) GitHubService {
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	return &gitHubService{
		token:   token,
		baseURL: baseURL,
		client:  &http.Client{},
	}
}

func (s *gitHubService) makeRequest(ctx context.Context, method, path string, body interface{}) (*http.Response, error) {
	var reqBody *bytes.Buffer
	if body != nil {
		jsonBody, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request body: %w", err)
		}
		reqBody = bytes.NewBuffer(jsonBody)
	} else {
		reqBody = &bytes.Buffer{}
	}

	url := fmt.Sprintf("%s/%s", strings.TrimRight(s.baseURL, "/"), path)

	// Get logger from context
	log := ctrl.LoggerFrom(ctx)
	log.V(1).Info("Making HTTP request", "method", method, "url", url)

	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}

	log.V(1).Info("HTTP response received", "status", resp.StatusCode)
	return resp, nil
}

func (s *gitHubService) CreateCostCenter(ctx context.Context, enterprise, name string) (*CostCenter, error) {
	path := fmt.Sprintf("enterprises/%s/settings/billing/cost-centers", enterprise)

	req := map[string]string{"name": name}

	resp, err := s.makeRequest(ctx, "POST", path, req)
	if err != nil {
		return nil, fmt.Errorf("failed to make HTTP request: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	// Read response body for error messages
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		bodyBytes = []byte("failed to read response body")
	}

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API error: %d %s - Response: %s", resp.StatusCode, resp.Status, string(bodyBytes))
	}

	var costCenter CostCenter
	if err := json.Unmarshal(bodyBytes, &costCenter); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w - Response: %s", err, string(bodyBytes))
	}

	return &costCenter, nil
}

func (s *gitHubService) GetCostCenter(ctx context.Context, enterprise, costCenterID string) (*CostCenter, error) {
	path := fmt.Sprintf("enterprises/%s/settings/billing/cost-centers/%s", enterprise, costCenterID)

	resp, err := s.makeRequest(ctx, "GET", path, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to make HTTP request: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode == http.StatusNotFound {
		return nil, &NotFoundError{Message: "cost center not found"}
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API error: %d %s - Response: %s", resp.StatusCode, resp.Status, string(bodyBytes))
	}

	// Log response for debugging
	log := ctrl.LoggerFrom(ctx)
	log.V(1).Info("GetCostCenter response received", "responseBody", string(bodyBytes))

	// The API returns a single object (not an array)
	var costCenter CostCenter
	if err := json.Unmarshal(bodyBytes, &costCenter); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w - Response: %s", err, string(bodyBytes))
	}

	return &costCenter, nil
}

func (s *gitHubService) ListCostCenters(ctx context.Context, enterprise string) ([]CostCenter, error) {
	path := fmt.Sprintf("enterprises/%s/settings/billing/cost-centers", enterprise)

	resp, err := s.makeRequest(ctx, "GET", path, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to make HTTP request: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API error: %d %s - Response: %s", resp.StatusCode, resp.Status, string(bodyBytes))
	}

	// Log response for debugging
	log := ctrl.LoggerFrom(ctx)
	log.V(1).Info("ListCostCenters response received", "responseBody", string(bodyBytes))

	// The API returns a wrapper object with a "costCenters" field
	type ListResponse struct {
		CostCenters []CostCenter `json:"costCenters"`
	}

	var response ListResponse
	if err := json.Unmarshal(bodyBytes, &response); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w - Response: %s", err, string(bodyBytes))
	}

	return response.CostCenters, nil
}

func (s *gitHubService) UpdateCostCenter(ctx context.Context, enterprise, costCenterID, name string) (*CostCenter, error) {
	path := fmt.Sprintf("enterprises/%s/settings/billing/cost-centers/%s", enterprise, costCenterID)

	req := map[string]string{"name": name}

	resp, err := s.makeRequest(ctx, "PATCH", path, req)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode == http.StatusNotFound {
		return nil, &NotFoundError{Message: "cost center not found for update"}
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API error: %d", resp.StatusCode)
	}

	// Read response body for flexible parsing
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	// Log response for debugging
	log := ctrl.LoggerFrom(ctx)
	log.V(1).Info("UpdateCostCenter response received", "responseBody", string(bodyBytes))

	// The API returns an array with one element
	var costCenters []CostCenter
	if err := json.Unmarshal(bodyBytes, &costCenters); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w - Response: %s", err, string(bodyBytes))
	}

	if len(costCenters) == 0 {
		return nil, &NotFoundError{Message: "cost center not found in update response"}
	}

	return &costCenters[0], nil
}

func (s *gitHubService) DeleteCostCenter(ctx context.Context, enterprise, costCenterID string) error {
	path := fmt.Sprintf("enterprises/%s/settings/billing/cost-centers/%s", enterprise, costCenterID)

	log := ctrl.LoggerFrom(ctx)
	log.Info("Making DELETE request to GitHub API", "enterprise", enterprise, "costCenterID", costCenterID, "path", path)

	resp, err := s.makeRequest(ctx, "DELETE", path, nil)
	if err != nil {
		log.Error(err, "Failed to make DELETE request to GitHub API")
		return fmt.Errorf("failed to delete cost center: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	log.Info("DELETE request completed", "statusCode", resp.StatusCode)

	// Read response body for debugging
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		bodyBytes = []byte("failed to read response body")
	}
	log.V(1).Info("DELETE response body", "responseBody", string(bodyBytes))

	if resp.StatusCode == http.StatusNotFound {
		log.Info("Cost center not found (404), treating as successfully deleted")
		return &NotFoundError{Message: "cost center not found"}
	}

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		log.Error(nil, "GitHub API returned error status", "statusCode", resp.StatusCode, "responseBody", string(bodyBytes))
		return fmt.Errorf("GitHub API error: %d - Response: %s", resp.StatusCode, string(bodyBytes))
	}

	log.Info("Successfully deleted cost center from GitHub")
	return nil
}

// NotFoundError represents a 404 error
type NotFoundError struct {
	Message string
}

func (e *NotFoundError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return "resource not found"
}

// containsFinalizer checks if a finalizer is present in the slice
func containsFinalizer(finalizers []string, finalizer string) bool {
	for _, f := range finalizers {
		if f == finalizer {
			return true
		}
	}
	return false
}

// DirectCostCenterReconciler bypasses the managed reconciler pattern to avoid upjet interference
type DirectCostCenterReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	Logger       logging.Logger
	newServiceFn func(ctx context.Context, token string, baseURL string) GitHubService
	recorder     event.Recorder
}

func (r *DirectCostCenterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Logger.WithValues("function", "DirectCostCenterReconciler.Reconcile", "resource", req.Name)
	log.Info("DIRECT CONTROLLER: Starting reconciliation")

	// Get the CostCenter resource
	var costCenter v1alpha1.CostCenter
	if err := r.Get(ctx, req.NamespacedName, &costCenter); err != nil {
		if client.IgnoreNotFound(err) == nil {
			log.Info("CostCenter resource not found, probably deleted")
			return ctrl.Result{}, nil
		}
		log.Info("Failed to get CostCenter resource", "error", err)
		return ctrl.Result{}, err
	}

	log.Info("DIRECT CONTROLLER: Found CostCenter resource",
		"name", costCenter.Spec.ForProvider.Name,
		"enterprise", costCenter.Spec.ForProvider.Enterprise,
		"deletionTimestamp", costCenter.GetDeletionTimestamp(),
		"finalizers", costCenter.GetFinalizers())

	// Check if the resource is being deleted
	if costCenter.GetDeletionTimestamp() != nil {
		log.Info("DIRECT CONTROLLER: Resource is being deleted, handling deletion")
		return r.handleDeletion(ctx, &costCenter)
	}

	// Add finalizer if it doesn't exist
	const finalizer = "finalizer.managedresource.crossplane.io"
	if !containsFinalizer(costCenter.GetFinalizers(), finalizer) {
		log.Info("DIRECT CONTROLLER: Adding finalizer")
		costCenter.SetFinalizers(append(costCenter.GetFinalizers(), finalizer))
		if err := r.Update(ctx, &costCenter); err != nil {
			log.Info("Failed to add finalizer", "error", err)
			return ctrl.Result{}, err
		}
		log.Info("DIRECT CONTROLLER: Successfully added finalizer, requeuing")
		return ctrl.Result{Requeue: true}, nil
	}

	// Get external client
	externalClient, err := r.getExternalClient(ctx, &costCenter)
	if err != nil {
		log.Info("Failed to create external client", "error", err)
		return ctrl.Result{RequeueAfter: time.Minute}, err
	}

	log.Info("DIRECT CONTROLLER: Successfully created external client")

	// Fetch the latest resource state to ensure we have current status
	if err := r.Get(ctx, req.NamespacedName, &costCenter); err != nil {
		log.Info("Failed to get latest CostCenter resource", "error", err)
		return ctrl.Result{RequeueAfter: time.Minute}, err
	}

	// Use the external client to observe the resource
	observation, err := externalClient.Observe(ctx, &costCenter)
	if err != nil {
		log.Info("Failed to observe external resource", "error", err)
		return ctrl.Result{RequeueAfter: time.Minute}, err
	}

	log.Info("DIRECT CONTROLLER: Observed external resource",
		"exists", observation.ResourceExists,
		"upToDate", observation.ResourceUpToDate)

	// Handle the resource based on its state
	if !observation.ResourceExists {
		// Create the resource
		log.Info("DIRECT CONTROLLER: Creating external resource")
		_, err := externalClient.Create(ctx, &costCenter)
		if err != nil {
			log.Info("Failed to create external resource", "error", err)
			return ctrl.Result{RequeueAfter: time.Minute}, err
		}
		log.Info("DIRECT CONTROLLER: Successfully created external resource")
	} else if !observation.ResourceUpToDate {
		// Update the resource
		log.Info("DIRECT CONTROLLER: Updating external resource")
		_, err := externalClient.Update(ctx, &costCenter)
		if err != nil {
			log.Info("Failed to update external resource", "error", err)
			return ctrl.Result{RequeueAfter: time.Minute}, err
		}
		log.Info("DIRECT CONTROLLER: Successfully updated external resource")
	}

	// Update the status to Ready and Synced
	costCenter.Status.SetConditions(xpv1.Available())
	if observation.ResourceUpToDate {
		costCenter.Status.SetConditions(xpv1.Available().WithMessage("Resource is up to date"))
	}

	// Set Synced condition - true if resource exists and is up to date
	if observation.ResourceExists {
		if observation.ResourceUpToDate {
			costCenter.Status.SetConditions(xpv1.ReconcileSuccess())
		} else {
			costCenter.Status.SetConditions(xpv1.ReconcileError(errors.New("resource exists but is not up to date")))
		}
	} else {
		costCenter.Status.SetConditions(xpv1.ReconcileError(errors.New("resource does not exist yet")))
	}

	// First update the status
	if err := r.Status().Update(ctx, &costCenter); err != nil {
		log.Info("Failed to update status", "error", err)
		return ctrl.Result{RequeueAfter: time.Minute}, err
	}

	// Fetch the resource again to get the latest version after status update
	var latestCostCenter v1alpha1.CostCenter
	if err := r.Get(ctx, req.NamespacedName, &latestCostCenter); err != nil {
		log.Info("Failed to get latest CostCenter resource for metadata update", "error", err)
		return ctrl.Result{RequeueAfter: time.Minute}, err
	}

	// Re-run observe to set the ExternalName annotation on the fresh resource
	_, err = externalClient.Observe(ctx, &latestCostCenter)
	if err != nil {
		log.Info("Failed to re-observe for ExternalName setting", "error", err)
		return ctrl.Result{RequeueAfter: time.Minute}, err
	}

	// Now update the resource metadata/spec (to persist ExternalName annotation)
	if err := r.Update(ctx, &latestCostCenter); err != nil {
		log.Info("Failed to update resource metadata", "error", err)
		return ctrl.Result{RequeueAfter: time.Minute}, err
	}

	log.Info("DIRECT CONTROLLER: Successfully set Ready and Synced conditions")
	return ctrl.Result{RequeueAfter: 10 * time.Minute}, nil
}

func (r *DirectCostCenterReconciler) handleDeletion(ctx context.Context, costCenter *v1alpha1.CostCenter) (ctrl.Result, error) {
	log := r.Logger.WithValues("function", "handleDeletion", "resource", costCenter.Name)
	log.Info("DIRECT CONTROLLER: Handling resource deletion")

	const finalizer = "finalizer.managedresource.crossplane.io"

	// Check if we have the finalizer
	if !containsFinalizer(costCenter.GetFinalizers(), finalizer) {
		log.Info("DIRECT CONTROLLER: No finalizer present, deletion already handled")
		return ctrl.Result{}, nil
	}

	// Get external client to perform deletion
	externalClient, err := r.getExternalClient(ctx, costCenter)
	if err != nil {
		log.Info("Failed to create external client for deletion", "error", err)
		// Continue with finalizer removal to avoid stuck resources
	} else {
		// Call the Delete method from our external client
		log.Info("DIRECT CONTROLLER: Calling external client Delete method")
		err = externalClient.Delete(ctx, costCenter)
		if err != nil {
			log.Info("Failed to delete external resource", "error", err)
			return ctrl.Result{RequeueAfter: time.Minute}, err
		}
		log.Info("DIRECT CONTROLLER: Successfully deleted external resource")
	}

	// Remove the finalizer to allow Kubernetes to delete the resource
	finalizers := costCenter.GetFinalizers()
	var newFinalizers []string
	for _, f := range finalizers {
		if f != finalizer {
			newFinalizers = append(newFinalizers, f)
		}
	}
	costCenter.SetFinalizers(newFinalizers)

	// Update the resource to remove the finalizer
	if err := r.Update(ctx, costCenter); err != nil {
		log.Info("Failed to remove finalizer", "error", err)
		return ctrl.Result{RequeueAfter: time.Minute}, err
	}

	log.Info("DIRECT CONTROLLER: Successfully removed finalizer, resource can now be deleted")
	return ctrl.Result{}, nil
}

func (r *DirectCostCenterReconciler) getExternalClient(ctx context.Context, cr *v1alpha1.CostCenter) (*external, error) {
	log := r.Logger.WithValues("function", "getExternalClient")
	log.Info("Creating external client for cost center")

	// Get provider config
	pc := &apisv1beta1.ProviderConfig{}
	if err := r.Get(ctx, types.NamespacedName{Name: cr.GetProviderConfigReference().Name}, pc); err != nil {
		return nil, errors.Wrap(err, errGetPC)
	}

	// Extract credentials
	cd := pc.Spec.Credentials
	data, err := resource.CommonCredentialExtractor(ctx, cd.Source, r.Client, cd.CommonCredentialSelectors)
	if err != nil {
		return nil, errors.Wrap(err, errGetCreds)
	}

	// Parse credentials
	type githubCreds struct {
		Token   *string `json:"token,omitempty"`
		BaseURL *string `json:"base_url,omitempty"`
	}

	var creds githubCreds
	if err := json.Unmarshal(data, &creds); err != nil {
		return nil, errors.Wrap(err, "failed to parse GitHub credentials JSON")
	}

	token := ""
	if creds.Token != nil {
		token = *creds.Token
	}

	if token == "" {
		return nil, errors.New("GitHub token is required but not provided in credentials")
	}

	baseURL := "https://api.github.com"
	if creds.BaseURL != nil && *creds.BaseURL != "" {
		baseURL = *creds.BaseURL
	}

	svc := r.newServiceFn(ctx, token, baseURL)
	return &external{service: svc}, nil
}
