# GitHub Enterprise Cost Centers API

This document describes the Cost Centers API implementation for GitHub Enterprise organizations.

## Overview

The Cost Centers API allows GitHub Enterprise administrators to create and manage cost centers for organizing billing and usage tracking. This implementation provides a Crossplane managed resource that interfaces with the GitHub REST API.

## Resource Definition

### CostCenter

A `CostCenter` represents a GitHub Enterprise cost center that can be used to organize resources for billing purposes.

#### Example

```yaml
apiVersion: enterprise.github.upbound.io/v1alpha1
kind: CostCenter
metadata:
  name: development-cost-center
spec:
  forProvider:
    enterprise: "my-enterprise"
    name: "Development Team"
  providerConfigRef:
    name: default
```

#### Fields

**Spec.ForProvider:**
- `enterprise` (string, required): The GitHub Enterprise slug
- `name` (string, required): The name of the cost center

**Status.AtProvider:**
- `id` (string): The GitHub-assigned ID of the cost center
- `name` (string): The name of the cost center 
- `state` (string): The current state of the cost center
- `resources` (array): Array of resources associated with this cost center
  - `type` (string): The type of resource
  - `name` (string): The name of the resource

## API Operations

The controller implements the following GitHub REST API operations:

- **Create**: `POST /enterprises/{enterprise}/settings/billing/cost-centers`
- **Read**: `GET /enterprises/{enterprise}/settings/billing/cost-centers/{cost_center_id}`
- **Update**: `PATCH /enterprises/{enterprise}/settings/billing/cost-centers/{cost_center_id}`
- **Delete**: `DELETE /enterprises/{enterprise}/settings/billing/cost-centers/{cost_center_id}`

## Authentication

The controller uses the same GitHub authentication as other resources in this provider:

1. Create a Personal Access Token with appropriate permissions
2. Store it in a Kubernetes Secret
3. Reference the secret in your ProviderConfig

Example ProviderConfig:

```yaml
apiVersion: pkg.crossplane.io/v1
kind: Provider
metadata:
  name: provider-upjet-github
spec:
  package: crossplane-contrib/provider-upjet-github:v0.5.0
---
apiVersion: github.upbound.io/v1beta1
kind: ProviderConfig
metadata:
  name: default
spec:
  credentials:
    source: Secret
    secretRef:
      namespace: crossplane-system
      name: github-secret
      key: token
```

## Permissions Required

The GitHub token used must have the following scopes for Enterprise organizations:
- `admin:enterprise` - For managing enterprise cost centers

## Implementation Notes

### GitHub API Compatibility

This implementation uses the GitHub REST API v2022-11-28. The cost centers API is available for GitHub Enterprise Cloud organizations.

### Resource Management

- Cost centers are created in the `active` state by default
- Updates only support changing the cost center name
- Deletion removes the cost center from the enterprise
- Associated resources are automatically unassigned when a cost center is deleted

### Error Handling

The controller handles the following GitHub API errors:
- `404 Not Found`: Resource does not exist (normal for deleted resources)
- `403 Forbidden`: Insufficient permissions
- `422 Unprocessable Entity`: Invalid request parameters

### Reconciliation

The controller polls the GitHub API every 10 minutes (configurable) to ensure the desired state matches the actual state.

## Troubleshooting

### Common Issues

1. **Permission Denied**: Ensure your GitHub token has `admin:enterprise` scope
2. **Enterprise Not Found**: Verify the enterprise slug is correct
3. **Resource Already Exists**: Cost center names must be unique within an enterprise

### Debugging

Enable debug logging by setting the log level in your provider deployment:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: provider-upjet-github
spec:
  template:
    spec:
      containers:
      - name: package-runtime
        args:
        - --debug
```

## Related APIs

- [GitHub Enterprise Organizations](../enterprise/organization.yaml)
- [Repository Management](../repository/)
- [Team Management](../team/)
