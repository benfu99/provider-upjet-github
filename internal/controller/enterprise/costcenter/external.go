/*
Copyright 2022 Upbound Inc.
*/

package costcenter

import (
	"context"

	"github.com/crossplane/crossplane-runtime/pkg/meta"
	"github.com/crossplane/crossplane-runtime/pkg/resource"
	"github.com/pkg/errors"

	"github.com/crossplane-contrib/provider-upjet-github/apis/enterprise/v1alpha1"
)

// ExternalObservation represents the result of observing an external resource
type ExternalObservation struct {
	ResourceExists          bool
	ResourceUpToDate        bool
	ResourceLateInitialized bool
	ConnectionDetails       map[string][]byte
}

// ExternalCreation represents the result of creating an external resource
type ExternalCreation struct {
	ConnectionDetails map[string][]byte
}

// ExternalUpdate represents the result of updating an external resource
type ExternalUpdate struct{}

// external implements the external client interface
type external struct {
	service GitHubService
}

func (e *external) Observe(ctx context.Context, mg resource.Managed) (ExternalObservation, error) {
	cr, ok := mg.(*v1alpha1.CostCenter)
	if !ok {
		return ExternalObservation{}, errors.New("managed resource is not a CostCenter")
	}

	// Always use ListCostCenters to get the state information
	// The GetCostCenter endpoint doesn't provide state, but ListCostCenters does
	costCenters, err := e.service.ListCostCenters(ctx, *cr.Spec.ForProvider.Enterprise)
	if err != nil {
		return ExternalObservation{}, errors.Wrap(err, "failed to list cost centers")
	}

	// Look for the cost center by ID first (if we have one), then by name
	var targetCostCenter *CostCenter

	if cr.Status.AtProvider.ID != nil {
		// Search by ID
		for _, cc := range costCenters {
			if cc.ID != nil && *cc.ID == *cr.Status.AtProvider.ID {
				targetCostCenter = &cc
				break
			}
		}
	}

	// If not found by ID, search by name
	if targetCostCenter == nil {
		for _, cc := range costCenters {
			if cc.Name != nil && *cc.Name == *cr.Spec.ForProvider.Name {
				targetCostCenter = &cc
				break
			}
		}
	}

	// If no cost center found
	if targetCostCenter == nil {
		return ExternalObservation{
			ResourceExists: false,
		}, nil
	}

	// Check if the cost center is in a deleted state
	if !e.isResourceActive(targetCostCenter) {
		// Cost center exists but is deleted, treat as non-existing
		return ExternalObservation{
			ResourceExists: false,
		}, nil
	}

	// Found matching active cost center
	e.updateStatus(cr, targetCostCenter)

	return ExternalObservation{
		ResourceExists:   true,
		ResourceUpToDate: e.isUpToDate(targetCostCenter, cr.Spec.ForProvider),
	}, nil
}

func (e *external) Create(ctx context.Context, mg resource.Managed) (ExternalCreation, error) {
	cr, ok := mg.(*v1alpha1.CostCenter)
	if !ok {
		return ExternalCreation{}, errors.New(errNotCostCenter)
	}

	enterprise := cr.Spec.ForProvider.Enterprise
	name := cr.Spec.ForProvider.Name
	if enterprise == nil || name == nil {
		return ExternalCreation{}, errors.New("enterprise and name must be specified")
	}

	costCenter, err := e.service.CreateCostCenter(ctx, *enterprise, *name)
	if err != nil {
		return ExternalCreation{}, err
	}

	// Update status and set external name
	e.updateStatus(cr, costCenter)

	return ExternalCreation{
		ConnectionDetails: map[string][]byte{
			"id":   []byte(*costCenter.ID),
			"name": []byte(*costCenter.Name),
		},
	}, nil
}

func (e *external) Update(ctx context.Context, mg resource.Managed) (ExternalUpdate, error) {
	cr, ok := mg.(*v1alpha1.CostCenter)
	if !ok {
		return ExternalUpdate{}, errors.New(errNotCostCenter)
	}

	enterprise := cr.Spec.ForProvider.Enterprise
	name := cr.Spec.ForProvider.Name
	id := cr.Status.AtProvider.ID

	if enterprise == nil || name == nil || id == nil {
		return ExternalUpdate{}, errors.New("enterprise, name, and id must be specified for update")
	}

	costCenter, err := e.service.UpdateCostCenter(ctx, *enterprise, *id, *name)
	if err != nil {
		return ExternalUpdate{}, err
	}

	// Update status
	e.updateStatus(cr, costCenter)

	return ExternalUpdate{}, nil
}

func (e *external) Delete(ctx context.Context, mg resource.Managed) error {
	cr, ok := mg.(*v1alpha1.CostCenter)
	if !ok {
		return errors.New(errNotCostCenter)
	}

	enterprise := cr.Spec.ForProvider.Enterprise
	id := cr.Status.AtProvider.ID

	if enterprise == nil || id == nil {
		return errors.New("enterprise and id must be specified for deletion")
	}

	return e.service.DeleteCostCenter(ctx, *enterprise, *id)
}

// updateStatus updates the status fields and sets the external name annotation
func (e *external) updateStatus(cr *v1alpha1.CostCenter, costCenter *CostCenter) {
	// Update AtProvider fields
	cr.Status.AtProvider.ID = costCenter.ID
	cr.Status.AtProvider.Name = costCenter.Name
	cr.Status.AtProvider.State = costCenter.State

	// Convert resources if present
	if costCenter.Resources != nil {
		cr.Status.AtProvider.Resources = make([]v1alpha1.CostCenterResource, len(costCenter.Resources))
		for i, res := range costCenter.Resources {
			cr.Status.AtProvider.Resources[i] = v1alpha1.CostCenterResource{
				Type: res.Type,
				Name: res.Name,
			}
		}
	}

	// Set external name annotation to the ID
	if costCenter.ID != nil {
		meta.SetExternalName(cr, *costCenter.ID)
	}
}

// isUpToDate checks if the external resource matches the desired state
func (e *external) isUpToDate(costCenter *CostCenter, spec v1alpha1.CostCenterParameters) bool {
	if spec.Name != nil && costCenter.Name != nil {
		return *spec.Name == *costCenter.Name
	}
	return true
}

// isResourceActive checks if the cost center is in an active (non-deleted) state
func (e *external) isResourceActive(costCenter *CostCenter) bool {
	if costCenter.State == nil {
		// If state is not provided, assume it's active
		return true
	}
	// Cost center is active if it's not in "deleted" state
	return *costCenter.State != "deleted"
}

// isNotFoundError checks if an error is a 404 not found error
func isNotFoundError(err error) bool {
	if _, ok := err.(*NotFoundError); ok {
		return true
	}
	return false
}
