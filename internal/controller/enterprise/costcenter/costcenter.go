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
	"github.com/crossplane/crossplane-runtime/pkg/meta"
	"github.com/crossplane/crossplane-runtime/pkg/reconciler/managed"
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
	name := managed.ControllerName(v1alpha1.CostCenterGroupVersionKind.String())

	o.Logger.Info("Setting up CostCenter controller", "name", name)

	// IMPORTANT: Try bypassing the managed reconciler pattern that might be interfering with upjet
	// Instead use a direct controller-runtime approach
	reconciler := &DirectCostCenterReconciler{
		Client:       mgr.GetClient(),
		Scheme:       mgr.GetScheme(),
		Logger:       o.Logger.WithValues("controller", name),
		newServiceFn: newGitHubService,
		recorder:     event.NewAPIRecorder(mgr.GetEventRecorderFor(name)),
	}

	err := ctrl.NewControllerManagedBy(mgr).
		Named(name + "-direct").
		WithOptions(o.ForControllerRuntime()).
		For(&v1alpha1.CostCenter{}).
		Complete(reconciler)

	if err != nil {
		o.Logger.Info("Failed to setup CostCenter controller", "error", err)
		return err
	}

	o.Logger.Info("Successfully completed CostCenter controller setup", "name", name)
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

// Helper function for safe string dereferencing
func getValue(ptr *string) string {
	if ptr != nil {
		return *ptr
	}
	return ""
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

	// Get logger from context for structured logging
	log := ctrl.LoggerFrom(ctx)
	log.V(1).Info("Making HTTP request", "method", method, "url", url)
	if body != nil {
		bodyJSON, err := json.Marshal(body)
		if err != nil {
			log.V(1).Info("Request body marshaling failed", "error", err)
		} else {
			log.V(1).Info("Request body", "body", string(bodyJSON))
		}
	}

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

	log.V(1).Info("HTTP response received", "status", resp.StatusCode, "statusText", resp.Status)
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

	// Log response for debugging
	log := ctrl.LoggerFrom(ctx)
	log.V(1).Info("CreateCostCenter response received", "responseBody", string(bodyBytes))

	var costCenter CostCenter
	if err := json.Unmarshal(bodyBytes, &costCenter); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w - Response: %s", err, string(bodyBytes))
	}

	// Log parsed cost center details
	log.V(1).Info("Parsed cost center",
		"id", getValue(costCenter.ID),
		"name", getValue(costCenter.Name),
		"state", getValue(costCenter.State))

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
		return nil, &NotFoundError{}
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API error: %d %s - Response: %s", resp.StatusCode, resp.Status, string(bodyBytes))
	}

	// Log response for debugging
	log := ctrl.LoggerFrom(ctx)
	log.V(1).Info("GetCostCenter response received", "responseBody", string(bodyBytes))

	// The API returns an array with one element
	var costCenters []CostCenter
	if err := json.Unmarshal(bodyBytes, &costCenters); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w - Response: %s", err, string(bodyBytes))
	}

	if len(costCenters) == 0 {
		return nil, &NotFoundError{}
	}

	return &costCenters[0], nil
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
		return nil, &NotFoundError{}
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
		return nil, &NotFoundError{}
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
		return err
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
		return &NotFoundError{}
	}

	if resp.StatusCode != http.StatusOK {
		log.Error(nil, "GitHub API returned error status", "statusCode", resp.StatusCode, "responseBody", string(bodyBytes))
		return fmt.Errorf("GitHub API error: %d - Response: %s", resp.StatusCode, string(bodyBytes))
	}

	log.Info("Successfully deleted cost center from GitHub")
	return nil
}

// NotFoundError represents a 404 error
type NotFoundError struct{}

func (e *NotFoundError) Error() string {
	return "resource not found"
}

// An ExternalClient observes, then either creates, updates, or deletes an
// external resource to ensure it reflects the managed resource's desired state.
type external struct {
	service  GitHubService
	recorder event.Recorder
}

func (c *external) Observe(ctx context.Context, mg resource.Managed) (managed.ExternalObservation, error) {
	cr, ok := mg.(*v1alpha1.CostCenter)
	if !ok {
		return managed.ExternalObservation{}, errors.New(errNotCostCenter)
	}

	log := ctrl.LoggerFrom(ctx).WithValues("function", "Observe")
	log.Info("Starting cost center observation", "name", cr.Name, "namespace", cr.Namespace)

	// Debug: Always log the current status ID value
	if cr.Status.AtProvider.ID != nil {
		log.Info("Status has ID", "statusID", *cr.Status.AtProvider.ID)
	} else {
		log.Info("Status has no ID - will check for existing resource")
	}

	// If we don't have an ID yet, check if resource exists in GitHub
	if cr.Status.AtProvider.ID == nil {
		return c.observeWithoutID(ctx, cr)
	}

	// We have an ID, get the resource from GitHub
	return c.observeWithID(ctx, cr)
}

// observeWithoutID handles observation when no ID is present (resource discovery)
func (c *external) observeWithoutID(ctx context.Context, cr *v1alpha1.CostCenter) (managed.ExternalObservation, error) {
	log := ctrl.LoggerFrom(ctx).WithValues("function", "observeWithoutID")
	log.Info("No ID found in status, checking if resource exists in GitHub")

	enterprise := cr.Spec.ForProvider.Enterprise
	name := cr.Spec.ForProvider.Name

	if enterprise == nil || name == nil {
		err := errors.New("enterprise and name must be specified")
		log.Error(err, "Missing required parameters for existence check")
		return managed.ExternalObservation{}, err
	}

	// Debug: Log what name we're actually searching for
	log.Info("Searching for cost center by name",
		"searchName", *name,
		"enterprise", *enterprise,
		"k8sResourceName", cr.Name)

	// Find existing cost center by name
	matchingCostCenter, err := c.findCostCenterByName(ctx, *enterprise, *name)
	if err != nil {
		c.recorder.Event(cr, event.Warning("ListFailed", err))
		return managed.ExternalObservation{}, err
	}

	if matchingCostCenter == nil {
		log.Info("No existing cost center found in GitHub")
		return managed.ExternalObservation{ResourceExists: false}, nil
	}

	// Adopt the existing resource
	return c.adoptExistingCostCenter(ctx, cr, matchingCostCenter)
}

// observeWithID handles observation when an ID is present
func (c *external) observeWithID(ctx context.Context, cr *v1alpha1.CostCenter) (managed.ExternalObservation, error) {
	log := ctrl.LoggerFrom(ctx).WithValues("function", "observeWithID")

	enterprise := cr.Spec.ForProvider.Enterprise
	if enterprise == nil {
		err := errors.New("enterprise must be specified")
		log.Error(err, "Missing enterprise specification")
		return managed.ExternalObservation{}, err
	}

	log.V(1).Info("Observing cost center with existing ID", "statusID", getValue(cr.Status.AtProvider.ID))
	log.Info("Getting cost center from GitHub API", "enterprise", *enterprise, "costCenterID", *cr.Status.AtProvider.ID)

	// Try to get the cost center
	costCenter, err := c.getCostCenterWithFallback(ctx, *enterprise, *cr.Status.AtProvider.ID)
	if err != nil {
		c.recorder.Event(cr, event.Warning("GetFailed", fmt.Errorf("both GetCostCenter and ListCostCenters failed: %w", err)))
		return managed.ExternalObservation{}, err
	}

	if costCenter == nil {
		c.recorder.Event(cr, event.Normal("ResourceNotFound", "Cost center not found in GitHub"))
		return managed.ExternalObservation{ResourceExists: false}, nil
	}

	// Update status and return observation
	c.updateCostCenterStatus(cr, costCenter)
	upToDate := isUpToDate(costCenter, cr.Spec.ForProvider)

	// Enhanced debugging for the Ready condition issue
	log.Info("Cost center observation complete",
		"upToDate", upToDate,
		"githubName", getValue(costCenter.Name),
		"specName", getValue(cr.Spec.ForProvider.Name),
		"githubState", getValue(costCenter.State),
		"statusID", getValue(cr.Status.AtProvider.ID))

	observation := managed.ExternalObservation{
		ResourceExists:          true,
		ResourceUpToDate:        upToDate,
		ResourceLateInitialized: false, // We don't late-initialize any fields
	}

	// Set connection details if needed (for Ready condition)
	if costCenter.ID != nil && costCenter.Name != nil {
		observation.ConnectionDetails = managed.ConnectionDetails{
			"id":   []byte(*costCenter.ID),
			"name": []byte(*costCenter.Name),
		}
	}

	log.Info("Returning observation",
		"resourceExists", observation.ResourceExists,
		"resourceUpToDate", observation.ResourceUpToDate,
		"resourceLateInitialized", observation.ResourceLateInitialized,
		"hasConnectionDetails", len(observation.ConnectionDetails) > 0)

	return observation, nil
}

// findCostCenterByName finds a cost center by name in the enterprise
func (c *external) findCostCenterByName(ctx context.Context, enterprise, name string) (*CostCenter, error) {
	log := ctrl.LoggerFrom(ctx).WithValues("function", "findCostCenterByName")

	log.Info("Listing cost centers to check for existing resource", "enterprise", enterprise, "name", name)
	costCenters, err := c.service.ListCostCenters(ctx, enterprise)
	if err != nil {
		log.Error(err, "Failed to list cost centers during existence check")
		return nil, errors.Wrap(err, "failed to check for existing cost center")
	}

	// Look for a cost center with matching name (prefer active ones, exclude deleted ones)
	var matchingCostCenter *CostCenter
	for _, cc := range costCenters {
		if cc.Name != nil && *cc.Name == name {
			// Skip deleted cost centers - they should not be considered as existing
			if cc.State != nil && *cc.State == "deleted" {
				continue
			}

			// If we haven't found one yet, or if this one is active and our current match isn't
			if matchingCostCenter == nil ||
				(cc.State != nil && *cc.State == "active" &&
					(matchingCostCenter.State == nil || *matchingCostCenter.State != "active")) {
				ccCopy := cc // Make a copy to avoid pointer issues
				matchingCostCenter = &ccCopy
			}
		}
	}

	return matchingCostCenter, nil
}

// adoptExistingCostCenter adopts an existing cost center found by name
func (c *external) adoptExistingCostCenter(ctx context.Context, cr *v1alpha1.CostCenter, costCenter *CostCenter) (managed.ExternalObservation, error) {
	log := ctrl.LoggerFrom(ctx).WithValues("function", "adoptExistingCostCenter")

	log.Info("Found existing cost center in GitHub, adopting it",
		"id", getValue(costCenter.ID),
		"name", getValue(costCenter.Name),
		"state", getValue(costCenter.State))
	c.recorder.Event(cr, event.Normal("AdoptedExistingResource", "Found and adopted existing cost center in GitHub"))

	// Update status
	c.updateCostCenterStatus(cr, costCenter)

	// Debug: Verify the status was updated
	if cr.Status.AtProvider.ID != nil {
		log.Info("Successfully updated status with ID", "statusID", *cr.Status.AtProvider.ID)
	} else {
		log.Error(nil, "Failed to update status - ID is still nil")
	}

	// Check if the resource is up to date
	upToDate := isUpToDate(costCenter, cr.Spec.ForProvider)
	log.Info("Adopted existing cost center",
		"upToDate", upToDate,
		"githubName", getValue(costCenter.Name),
		"specName", getValue(cr.Spec.ForProvider.Name))

	observation := managed.ExternalObservation{
		ResourceExists:          true,
		ResourceUpToDate:        upToDate,
		ResourceLateInitialized: false, // We don't late-initialize any fields
	}

	// Set connection details if needed (for Ready condition)
	if costCenter.ID != nil && costCenter.Name != nil {
		observation.ConnectionDetails = managed.ConnectionDetails{
			"id":   []byte(*costCenter.ID),
			"name": []byte(*costCenter.Name),
		}
	}

	log.Info("Returning adoption observation",
		"resourceExists", observation.ResourceExists,
		"resourceUpToDate", observation.ResourceUpToDate,
		"resourceLateInitialized", observation.ResourceLateInitialized,
		"hasConnectionDetails", len(observation.ConnectionDetails) > 0)

	return observation, nil
}

// getCostCenterWithFallback tries to get a cost center by ID with fallback to list search
func (c *external) getCostCenterWithFallback(ctx context.Context, enterprise, id string) (*CostCenter, error) {
	log := ctrl.LoggerFrom(ctx).WithValues("function", "getCostCenterWithFallback")

	costCenter, err := c.service.GetCostCenter(ctx, enterprise, id)
	if err != nil {
		log.Info("GetCostCenter failed, checking error type", "error", err.Error())

		// If it's a 404, the resource doesn't exist
		if IsNotFound(err) {
			log.Info("Cost center not found in GitHub, marking as non-existent")
			return nil, nil
		}

		// Try fallback via list
		return c.getCostCenterFromList(ctx, enterprise, id)
	}

	log.V(1).Info("Successfully retrieved cost center from GitHub via GetCostCenter")
	return costCenter, nil
}

// getCostCenterFromList finds a cost center by ID in the list (fallback method)
func (c *external) getCostCenterFromList(ctx context.Context, enterprise, id string) (*CostCenter, error) {
	log := ctrl.LoggerFrom(ctx).WithValues("function", "getCostCenterFromList")

	log.Info("GetCostCenter failed, attempting to verify via ListCostCenters")

	costCenters, listErr := c.service.ListCostCenters(ctx, enterprise)
	if listErr != nil {
		log.Error(listErr, "Failed to list cost centers during fallback verification")
		return nil, errors.Wrap(listErr, errGetCostCenter)
	}

	// Look for the cost center by ID in the list (exclude deleted ones)
	for _, cc := range costCenters {
		if cc.ID != nil && *cc.ID == id {
			// If we find the cost center but it's deleted, treat it as non-existent
			if cc.State != nil && *cc.State == "deleted" {
				log.Info("Cost center found but marked as deleted, treating as non-existent", "id", id, "state", *cc.State)
				return nil, nil
			}

			ccCopy := cc // Make a copy to avoid pointer issues
			log.Info("Successfully verified cost center via ListCostCenters fallback")
			return &ccCopy, nil
		}
	}

	log.Info("Cost center not found in list, marking as non-existent")
	return nil, nil
}

// updateCostCenterStatus updates the CR status with cost center data
func (c *external) updateCostCenterStatus(cr *v1alpha1.CostCenter, costCenter *CostCenter) {
	cr.Status.AtProvider.ID = costCenter.ID
	cr.Status.AtProvider.Name = costCenter.Name
	cr.Status.AtProvider.State = costCenter.State

	// Set the external name to the actual cost center name returned from GitHub API
	if costCenter.Name != nil {
		meta.SetExternalName(cr, *costCenter.Name)
	}

	// Convert resources
	if costCenter.Resources != nil {
		cr.Status.AtProvider.Resources = make([]v1alpha1.CostCenterResource, len(costCenter.Resources))
		for i, res := range costCenter.Resources {
			cr.Status.AtProvider.Resources[i] = v1alpha1.CostCenterResource{
				Type: res.Type,
				Name: res.Name,
			}
		}
	}
}

// handleExistingCostCenter handles the case where a cost center already exists (409 conflict)
func (c *external) handleExistingCostCenter(ctx context.Context, cr *v1alpha1.CostCenter, enterprise, name string, _ error) (managed.ExternalCreation, error) {
	log := ctrl.LoggerFrom(ctx).WithValues("function", "handleExistingCostCenter")
	log.Info("Cost center already exists, will be adopted on next observation", "enterprise", enterprise, "name", name)

	// Don't try to find and adopt the resource here - let the next Observe call handle it
	// The key insight is that we should return success from Create and let Observe discover the resource
	c.recorder.Event(cr, event.Normal("ExistingResourceDetected", "Cost center already exists and will be adopted"))

	// Return success - the next Observe call will find and adopt the existing resource
	return managed.ExternalCreation{}, nil
}

func (c *external) Create(ctx context.Context, mg resource.Managed) (managed.ExternalCreation, error) {
	cr, ok := mg.(*v1alpha1.CostCenter)
	if !ok {
		return managed.ExternalCreation{}, errors.New(errNotCostCenter)
	}

	log := ctrl.LoggerFrom(ctx).WithValues("function", "Create")
	log.Info("Starting cost center creation", "name", cr.Name, "namespace", cr.Namespace)

	enterprise := cr.Spec.ForProvider.Enterprise
	name := cr.Spec.ForProvider.Name

	if enterprise == nil || name == nil {
		err := errors.New("enterprise and name must be specified")
		log.Error(err, "Missing required parameters", "enterprise", enterprise, "name", name)
		return managed.ExternalCreation{}, err
	}

	log.Info("Creating cost center in GitHub", "enterprise", *enterprise, "name", *name)

	costCenter, err := c.service.CreateCostCenter(ctx, *enterprise, *name)
	if err != nil {
		// Check if it's a 409 conflict (cost center already exists)
		if strings.Contains(err.Error(), "409") && strings.Contains(err.Error(), "already exists") {
			return c.handleExistingCostCenter(ctx, cr, *enterprise, *name, err)
		}

		log.Error(err, "Failed to create cost center in GitHub API")
		c.recorder.Event(cr, event.Warning("CreateFailed", err))
		return managed.ExternalCreation{}, errors.Wrap(err, errCreateCostCenter)
	}

	// Helper function to safely get string value from pointer
	getValue := func(ptr *string) string {
		if ptr != nil {
			return *ptr
		}
		return ""
	}

	log.Info("Successfully created cost center",
		"id", getValue(costCenter.ID),
		"name", getValue(costCenter.Name),
		"state", getValue(costCenter.State))
	c.recorder.Event(cr, event.Normal("Created", "Successfully created cost center in GitHub"))

	// Update the status with the new ID
	cr.Status.AtProvider.ID = costCenter.ID
	cr.Status.AtProvider.Name = costCenter.Name
	cr.Status.AtProvider.State = costCenter.State

	// Set the external name to the actual cost center name returned from GitHub API
	if costCenter.Name != nil {
		meta.SetExternalName(cr, *costCenter.Name)
	}

	// Log the ID was set
	log.Info("Set cost center ID in status", "statusID", getValue(cr.Status.AtProvider.ID))

	// Return creation result with connection details
	creation := managed.ExternalCreation{}
	if costCenter.ID != nil && costCenter.Name != nil {
		creation.ConnectionDetails = managed.ConnectionDetails{
			"id":   []byte(*costCenter.ID),
			"name": []byte(*costCenter.Name),
		}
		log.Info("Returning creation with connection details", "hasConnectionDetails", true)
	}

	return creation, nil
}

func (c *external) Update(ctx context.Context, mg resource.Managed) (managed.ExternalUpdate, error) {
	cr, ok := mg.(*v1alpha1.CostCenter)
	if !ok {
		return managed.ExternalUpdate{}, errors.New(errNotCostCenter)
	}

	enterprise := cr.Spec.ForProvider.Enterprise
	name := cr.Spec.ForProvider.Name
	costCenterID := cr.Status.AtProvider.ID

	if enterprise == nil || name == nil || costCenterID == nil {
		return managed.ExternalUpdate{}, errors.New("enterprise, name, and cost center ID must be specified")
	}

	costCenter, err := c.service.UpdateCostCenter(ctx, *enterprise, *costCenterID, *name)
	if err != nil {
		return managed.ExternalUpdate{}, errors.Wrap(err, errUpdateCostCenter)
	}

	// Update the status
	cr.Status.AtProvider.Name = costCenter.Name
	cr.Status.AtProvider.State = costCenter.State

	// Set the external name to the actual cost center name returned from GitHub API
	if costCenter.Name != nil {
		meta.SetExternalName(cr, *costCenter.Name)
	}

	return managed.ExternalUpdate{}, nil
}

func (c *external) Delete(ctx context.Context, mg resource.Managed) error {
	cr, ok := mg.(*v1alpha1.CostCenter)
	if !ok {
		return errors.New(errNotCostCenter)
	}

	log := ctrl.LoggerFrom(ctx).WithValues("function", "Delete")
	log.Info("Starting cost center deletion", "name", cr.Name, "namespace", cr.Namespace)

	enterprise := cr.Spec.ForProvider.Enterprise
	costCenterID := cr.Status.AtProvider.ID

	if enterprise == nil || costCenterID == nil {
		log.Info("No enterprise or cost center ID provided, skipping deletion")
		return nil // Nothing to delete
	}

	log.Info("Deleting cost center from GitHub", "enterprise", *enterprise, "costCenterID", *costCenterID)

	err := c.service.DeleteCostCenter(ctx, *enterprise, *costCenterID)
	if err != nil {
		if IsNotFound(err) {
			log.Info("Cost center not found in GitHub, considering deletion successful")
			c.recorder.Event(cr, event.Normal("DeleteCompleted", "Cost center not found in GitHub (already deleted)"))
		} else {
			log.Error(err, "Failed to delete cost center from GitHub")
			c.recorder.Event(cr, event.Warning("DeleteFailed", err))
			return errors.Wrap(err, errDeleteCostCenter)
		}
	} else {
		log.Info("Successfully deleted cost center from GitHub")
		c.recorder.Event(cr, event.Normal("DeleteCompleted", "Successfully deleted cost center from GitHub"))
	}

	return nil
}

// Helper functions

func isUpToDate(costCenter *CostCenter, params v1alpha1.CostCenterParameters) bool {
	if costCenter.Name == nil || params.Name == nil {
		return false
	}

	actualName := *costCenter.Name
	desiredName := *params.Name
	isMatch := actualName == desiredName

	return isMatch
}

// IsNotFound checks if the given error is a NotFoundError
func IsNotFound(err error) bool {
	if err == nil {
		return false
	}
	var notFoundErr *NotFoundError
	return errors.As(err, &notFoundErr)
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
		"deletionTimestamp", costCenter.GetDeletionTimestamp())

	// Check if the resource is being deleted
	if costCenter.GetDeletionTimestamp() != nil {
		log.Info("DIRECT CONTROLLER: Resource is being deleted, handling deletion")
		return r.handleDeletion(ctx, &costCenter)
	}

	// Get external client
	externalClient, err := r.getExternalClient(ctx, &costCenter)
	if err != nil {
		log.Info("Failed to create external client", "error", err)
		return ctrl.Result{RequeueAfter: time.Minute}, err
	}

	log.Info("DIRECT CONTROLLER: Successfully created external client")

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

	if err := r.Status().Update(ctx, &costCenter); err != nil {
		log.Info("Failed to update status", "error", err)
		return ctrl.Result{RequeueAfter: time.Minute}, err
	}

	log.Info("DIRECT CONTROLLER: Successfully set Ready and Synced conditions")
	return ctrl.Result{RequeueAfter: 10 * time.Minute}, nil
}

func (r *DirectCostCenterReconciler) handleDeletion(ctx context.Context, costCenter *v1alpha1.CostCenter) (ctrl.Result, error) {
	log := r.Logger.WithValues("function", "handleDeletion", "resource", costCenter.Name)
	log.Info("DIRECT CONTROLLER: Handling resource deletion")

	// Get external client to perform deletion
	externalClient, err := r.getExternalClient(ctx, costCenter)
	if err != nil {
		log.Info("Failed to create external client for deletion", "error", err)
		return ctrl.Result{RequeueAfter: time.Minute}, err
	}

	// Call the Delete method from our external client
	log.Info("DIRECT CONTROLLER: Calling external client Delete method")
	err = externalClient.Delete(ctx, costCenter)
	if err != nil {
		log.Info("Failed to delete external resource", "error", err)
		return ctrl.Result{RequeueAfter: time.Minute}, err
	}

	log.Info("DIRECT CONTROLLER: Successfully deleted external resource")

	// Remove the finalizer to allow Kubernetes to delete the resource
	finalizers := costCenter.GetFinalizers()
	var newFinalizers []string
	for _, finalizer := range finalizers {
		if finalizer != "finalizer.managedresource.crossplane.io" {
			newFinalizers = append(newFinalizers, finalizer)
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
	return &external{service: svc, recorder: r.recorder}, nil
}
