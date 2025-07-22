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

	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane/crossplane-runtime/pkg/event"
	"github.com/crossplane/crossplane-runtime/pkg/ratelimiter"
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

	cps := []managed.ConnectionPublisher{managed.NewAPISecretPublisher(mgr.GetClient(), mgr.GetScheme())}

	recorder := event.NewAPIRecorder(mgr.GetEventRecorderFor(name))

	r := managed.NewReconciler(mgr,
		resource.ManagedKind(v1alpha1.CostCenterGroupVersionKind),
		managed.WithExternalConnecter(&connector{
			kube:         mgr.GetClient(),
			usage:        resource.NewProviderConfigUsageTracker(mgr.GetClient(), &apisv1beta1.ProviderConfigUsage{}),
			newServiceFn: newGitHubService,
			recorder:     recorder,
		}),
		managed.WithLogger(o.Logger.WithValues("controller", name)),
		managed.WithPollInterval(o.PollInterval),
		managed.WithRecorder(recorder),
		managed.WithConnectionPublishers(cps...))

	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		WithOptions(o.ForControllerRuntime()).
		WithEventFilter(resource.DesiredStateChanged()).
		For(&v1alpha1.CostCenter{}).
		Complete(ratelimiter.NewReconciler(name, r, o.GlobalRateLimiter))
}

// A connector is expected to produce an ExternalClient when its Connect method
// is called.
type connector struct {
	kube         client.Client
	usage        resource.Tracker
	newServiceFn func(ctx context.Context, token string, baseURL string) GitHubService
	recorder     event.Recorder
}

// Connect typically produces an ExternalClient by:
// 1. Tracking that the managed resource is using a ProviderConfig.
// 2. Getting the managed resource's ProviderConfig.
// 3. Getting the credentials specified by the ProviderConfig.
// 4. Using the credentials to form a client.
func (c *connector) Connect(ctx context.Context, mg resource.Managed) (managed.ExternalClient, error) {
	cr, ok := mg.(*v1alpha1.CostCenter)
	if !ok {
		return nil, errors.New(errNotCostCenter)
	}

	if err := c.usage.Track(ctx, mg); err != nil {
		return nil, errors.Wrap(err, errTrackPCUsage)
	}

	pc := &apisv1beta1.ProviderConfig{}
	if err := c.kube.Get(ctx, types.NamespacedName{Name: cr.GetProviderConfigReference().Name}, pc); err != nil {
		return nil, errors.Wrap(err, errGetPC)
	}

	cd := pc.Spec.Credentials
	data, err := resource.CommonCredentialExtractor(ctx, cd.Source, c.kube, cd.CommonCredentialSelectors)
	if err != nil {
		return nil, errors.Wrap(err, errGetCreds)
	}

	// Parse credentials as JSON
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

	svc := c.newServiceFn(ctx, token, baseURL)
	return &external{service: svc, recorder: c.recorder}, nil
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

	resp, err := s.makeRequest(ctx, "DELETE", path, nil)
	if err != nil {
		return err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode == http.StatusNotFound {
		return &NotFoundError{}
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub API error: %d", resp.StatusCode)
	}

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
	log.Info("Cost center observation complete", "upToDate", upToDate)

	return managed.ExternalObservation{
		ResourceExists:   true,
		ResourceUpToDate: upToDate,
	}, nil
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

	// Look for a cost center with matching name (prefer active ones)
	var matchingCostCenter *CostCenter
	for _, cc := range costCenters {
		if cc.Name != nil && *cc.Name == name {
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

	return managed.ExternalObservation{
		ResourceExists:   true,
		ResourceUpToDate: upToDate,
	}, nil
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

	// Look for the cost center by ID in the list
	for _, cc := range costCenters {
		if cc.ID != nil && *cc.ID == id {
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

	// Log the ID was set
	log.V(1).Info("Set cost center ID in status", "statusID", getValue(cr.Status.AtProvider.ID))

	return managed.ExternalCreation{}, nil
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

	return managed.ExternalUpdate{}, nil
}

func (c *external) Delete(ctx context.Context, mg resource.Managed) error {
	cr, ok := mg.(*v1alpha1.CostCenter)
	if !ok {
		return errors.New(errNotCostCenter)
	}

	enterprise := cr.Spec.ForProvider.Enterprise
	costCenterID := cr.Status.AtProvider.ID

	if enterprise == nil || costCenterID == nil {
		return nil // Nothing to delete
	}

	err := c.service.DeleteCostCenter(ctx, *enterprise, *costCenterID)
	if err != nil && !IsNotFound(err) {
		return errors.Wrap(err, errDeleteCostCenter)
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

	// This function is called frequently, so use a global logger approach
	// since we don't have context here
	if !isMatch {
		// Log names for debugging name mismatch issues
		fmt.Printf("DEBUG: Name mismatch - GitHub: '%s' vs Spec: '%s'\n", actualName, desiredName)
	}

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
