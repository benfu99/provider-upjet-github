/*
Copyright 2022 Upbound Inc.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	v1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
)

// CostCenterResource represents a resource associated with a cost center
type CostCenterResource struct {
	// The type of the resource (User, Repo, Organization)
	Type *string `json:"type,omitempty"`

	// The name of the resource
	Name *string `json:"name,omitempty"`
}

// CostCenterInitParameters defines the desired state for initialization
type CostCenterInitParameters struct {
	// The name of the enterprise
	// +kubebuilder:validation:Required
	Enterprise *string `json:"enterprise,omitempty"`

	// The name of the cost center (max length 255 characters)
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=255
	Name *string `json:"name,omitempty"`
}

// CostCenterObservation defines the observed state of CostCenter
type CostCenterObservation struct {
	// The unique identifier of the cost center
	ID *string `json:"id,omitempty"`

	// The name of the cost center
	Name *string `json:"name,omitempty"`

	// The state of the cost center (active, archived)
	State *string `json:"state,omitempty"`

	// The resources associated with the cost center
	Resources []CostCenterResource `json:"resources,omitempty"`
}

// CostCenterParameters defines the desired state parameters
type CostCenterParameters struct {
	// The name of the enterprise
	// +kubebuilder:validation:Required
	Enterprise *string `json:"enterprise,omitempty"`

	// Reference to an Enterprise to populate enterprise.
	// +kubebuilder:validation:Optional
	EnterpriseRef *v1.Reference `json:"enterpriseRef,omitempty"`

	// Selector for an Enterprise to populate enterprise.
	// +kubebuilder:validation:Optional
	EnterpriseSelector *v1.Selector `json:"enterpriseSelector,omitempty"`

	// The name of the cost center (max length 255 characters)
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=255
	Name *string `json:"name"`
}

// CostCenterSpec defines the desired state of CostCenter
type CostCenterSpec struct {
	v1.ResourceSpec `json:",inline"`
	ForProvider     CostCenterParameters `json:"forProvider"`
}

// CostCenterStatus defines the observed state of CostCenter
type CostCenterStatus struct {
	v1.ResourceStatus `json:",inline"`
	AtProvider        CostCenterObservation `json:"atProvider,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:storageversion
// +kubebuilder:printcolumn:name="READY",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="SYNCED",type="string",JSONPath=".status.conditions[?(@.type=='Synced')].status"
// +kubebuilder:printcolumn:name="EXTERNAL-NAME",type="string",JSONPath=".metadata.annotations.crossplane\\.io/external-name"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:resource:scope=Cluster,categories={crossplane,managed,github}

// CostCenter is the Schema for the CostCenters API. Manages a GitHub Enterprise Cost Center.
type CostCenter struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CostCenterSpec   `json:"spec"`
	Status CostCenterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// CostCenterList contains a list of CostCenters
type CostCenterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CostCenter `json:"items"`
}

// Repository type metadata.
var (
	CostCenter_Kind             = "CostCenter"
	CostCenter_GroupKind        = schema.GroupKind{Group: CRDGroup, Kind: CostCenter_Kind}.String()
	CostCenter_KindAPIVersion   = CostCenter_Kind + "." + CRDGroupVersion.String()
	CostCenter_GroupVersionKind = CRDGroupVersion.WithKind(CostCenter_Kind)
)

func init() {
	SchemeBuilder.Register(&CostCenter{}, &CostCenterList{})
}
