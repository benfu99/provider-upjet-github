/*
Copyright 2022 Upbound Inc.
*/

package costcenter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

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

	cps := []managed.ConnectionPublisher{managed.NewAPISecretPublisher(mgr.GetClient(), mgr.GetScheme())}

	r := managed.NewReconciler(mgr,
		resource.ManagedKind(v1alpha1.CostCenterGroupVersionKind),
		managed.WithExternalConnecter(&connector{
			kube:         mgr.GetClient(),
			usage:        resource.NewProviderConfigUsageTracker(mgr.GetClient(), &apisv1beta1.ProviderConfigUsage{}),
			newServiceFn: newGitHubService,
		}),
		managed.WithLogger(o.Logger.WithValues("controller", name)),
		managed.WithPollInterval(o.PollInterval),
		managed.WithRecorder(event.NewAPIRecorder(mgr.GetEventRecorderFor(name))),
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
		return nil, errors.Wrap(err, errGetCreds)
	}

	token := ""
	if creds.Token != nil {
		token = *creds.Token
	}

	baseURL := ""
	if creds.BaseURL != nil {
		baseURL = *creds.BaseURL
	}

	svc := c.newServiceFn(ctx, token, baseURL)
	return &external{service: svc}, nil
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
			return nil, err
		}
		reqBody = bytes.NewBuffer(jsonBody)
	} else {
		reqBody = &bytes.Buffer{}
	}

	url := fmt.Sprintf("%s/%s", s.baseURL, path)
	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	return s.client.Do(req)
}

func (s *gitHubService) CreateCostCenter(ctx context.Context, enterprise, name string) (*CostCenter, error) {
	path := fmt.Sprintf("enterprises/%s/settings/billing/cost-centers", enterprise)

	req := map[string]string{"name": name}

	resp, err := s.makeRequest(ctx, "POST", path, req)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API error: %d", resp.StatusCode)
	}

	var costCenter CostCenter
	if err := json.NewDecoder(resp.Body).Decode(&costCenter); err != nil {
		return nil, err
	}

	return &costCenter, nil
}

func (s *gitHubService) GetCostCenter(ctx context.Context, enterprise, costCenterID string) (*CostCenter, error) {
	path := fmt.Sprintf("enterprises/%s/settings/billing/cost-centers/%s", enterprise, costCenterID)

	resp, err := s.makeRequest(ctx, "GET", path, nil)
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

	// The API returns an array with one element
	var costCenters []CostCenter
	if err := json.NewDecoder(resp.Body).Decode(&costCenters); err != nil {
		return nil, err
	}

	if len(costCenters) == 0 {
		return nil, &NotFoundError{}
	}

	return &costCenters[0], nil
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

	// The API returns an array with one element
	var costCenters []CostCenter
	if err := json.NewDecoder(resp.Body).Decode(&costCenters); err != nil {
		return nil, err
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
	service GitHubService
}

func (c *external) Observe(ctx context.Context, mg resource.Managed) (managed.ExternalObservation, error) {
	cr, ok := mg.(*v1alpha1.CostCenter)
	if !ok {
		return managed.ExternalObservation{}, errors.New(errNotCostCenter)
	}

	// If we don't have an ID yet, the resource doesn't exist
	if cr.Status.AtProvider.ID == nil {
		return managed.ExternalObservation{
			ResourceExists: false,
		}, nil
	}

	enterprise := cr.Spec.ForProvider.Enterprise
	if enterprise == nil {
		return managed.ExternalObservation{}, errors.New("enterprise must be specified")
	}

	costCenter, err := c.service.GetCostCenter(ctx, *enterprise, *cr.Status.AtProvider.ID)
	if err != nil {
		// If it's a 404, the resource doesn't exist
		if IsNotFound(err) {
			return managed.ExternalObservation{
				ResourceExists: false,
			}, nil
		}
		return managed.ExternalObservation{}, errors.Wrap(err, errGetCostCenter)
	}

	// Update the status with the current state
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

	// Check if the resource is up to date
	upToDate := isUpToDate(costCenter, cr.Spec.ForProvider)

	return managed.ExternalObservation{
		ResourceExists:   true,
		ResourceUpToDate: upToDate,
	}, nil
}

func (c *external) Create(ctx context.Context, mg resource.Managed) (managed.ExternalCreation, error) {
	cr, ok := mg.(*v1alpha1.CostCenter)
	if !ok {
		return managed.ExternalCreation{}, errors.New(errNotCostCenter)
	}

	enterprise := cr.Spec.ForProvider.Enterprise
	name := cr.Spec.ForProvider.Name

	if enterprise == nil || name == nil {
		return managed.ExternalCreation{}, errors.New("enterprise and name must be specified")
	}

	costCenter, err := c.service.CreateCostCenter(ctx, *enterprise, *name)
	if err != nil {
		return managed.ExternalCreation{}, errors.Wrap(err, errCreateCostCenter)
	}

	// Update the status with the new ID
	cr.Status.AtProvider.ID = costCenter.ID
	cr.Status.AtProvider.Name = costCenter.Name
	cr.Status.AtProvider.State = costCenter.State

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
	return *costCenter.Name == *params.Name
}

// IsNotFound checks if the given error is a NotFoundError
func IsNotFound(err error) bool {
	if err == nil {
		return false
	}
	var notFoundErr *NotFoundError
	return errors.As(err, &notFoundErr)
}
