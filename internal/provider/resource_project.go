package provider

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/argoproj-labs/terraform-provider-argocd/internal/diagnostics"
	"github.com/argoproj-labs/terraform-provider-argocd/internal/features"
	argocdSync "github.com/argoproj-labs/terraform-provider-argocd/internal/sync"
	"github.com/argoproj/argo-cd/v3/pkg/apiclient/project"
	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Ensure provider defined types fully satisfy framework interfaces.
var _ resource.Resource = &projectResource{}

func NewProjectResource() resource.Resource {
	return &projectResource{}
}

type projectResource struct {
	si *ServerInterface
}

func (r *projectResource) Metadata(ctx context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_project"
}

func (r *projectResource) Schema(ctx context.Context, req resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages [projects](https://argo-cd.readthedocs.io/en/stable/user-guide/projects/) within ArgoCD.",
		Blocks:              projectSchemaBlocks(),
	}
}

func (r *projectResource) Configure(ctx context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	// Prevent panic if the provider has not been configured.
	if req.ProviderData == nil {
		return
	}

	si, ok := req.ProviderData.(*ServerInterface)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Provider Data Type",
			fmt.Sprintf("Expected *ServerInterface, got: %T. Please report this issue to the provider developers.", req.ProviderData),
		)
		return
	}

	r.si = si
}

func (r *projectResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data projectModel

	// Read Terraform configuration data into the model
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)

	// Initialize API clients
	resp.Diagnostics.Append(r.si.InitClients(ctx)...)

	// Check for errors before proceeding
	if resp.Diagnostics.HasError() {
		return
	}

	// Convert model to ArgoCD project
	objectMeta, spec, diags := expandProject(ctx, &data)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	projectName := objectMeta.Name

	// Check feature support
	if !r.si.IsFeatureSupported(features.ProjectSourceNamespaces) && len(data.Spec.SourceNamespaces) > 0 {
		resp.Diagnostics.Append(diagnostics.FeatureNotSupported(features.ProjectSourceNamespaces)...)
		return
	}

	if !r.si.IsFeatureSupported(features.ProjectDestinationServiceAccounts) && len(data.Spec.DestinationServiceAccount) > 0 {
		resp.Diagnostics.Append(diagnostics.FeatureNotSupported(features.ProjectDestinationServiceAccounts)...)
		return
	}

	// Initialize project mutex
	if _, ok := argocdSync.TokenMutexProjectMap[projectName]; !ok {
		argocdSync.TokenMutexProjectMap[projectName] = &sync.RWMutex{}
	}

	argocdSync.TokenMutexProjectMap[projectName].Lock()
	defer argocdSync.TokenMutexProjectMap[projectName].Unlock()

	// Check if project already exists
	p, err := r.si.ProjectClient.Get(ctx, &project.ProjectQuery{
		Name: projectName,
	})
	if err != nil && !strings.Contains(err.Error(), "NotFound") {
		resp.Diagnostics.Append(diagnostics.ArgoCDAPIError("get", "project", projectName, err)...)
		return
	} else if p != nil {
		switch p.DeletionTimestamp {
		case nil:
		default:
			// Pre-existing project is still in Kubernetes soft deletion queue
			if p.DeletionGracePeriodSeconds != nil {
				time.Sleep(time.Duration(*p.DeletionGracePeriodSeconds) * time.Second)
			}
		}
	}

	// Create project
	p, err = r.si.ProjectClient.Create(ctx, &project.ProjectCreateRequest{
		Project: &v1alpha1.AppProject{
			ObjectMeta: objectMeta,
			Spec:       spec,
		},
		Upsert: false,
	})

	if err != nil {
		resp.Diagnostics.Append(diagnostics.ArgoCDAPIError("create", "project", projectName, err)...)
		return
	} else if p == nil {
		resp.Diagnostics.AddError(
			"Project Creation Failed",
			fmt.Sprintf("project %s could not be created: unknown reason", projectName),
		)
		return
	}

	tflog.Trace(ctx, fmt.Sprintf("created project %s", projectName))

	// Parse response and store state
	resp.Diagnostics.Append(resp.State.Set(ctx, newProject(p))...)
}

func (r *projectResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data projectModel

	// Read Terraform prior state data into the model
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)

	// Initialize API clients
	resp.Diagnostics.Append(r.si.InitClients(ctx)...)

	// Check for errors before proceeding
	if resp.Diagnostics.HasError() {
		return
	}

	projectName := data.Metadata.Name.ValueString()

	// Initialize project mutex
	if _, ok := argocdSync.TokenMutexProjectMap[projectName]; !ok {
		argocdSync.TokenMutexProjectMap[projectName] = &sync.RWMutex{}
	}

	argocdSync.TokenMutexProjectMap[projectName].RLock()
	p, err := r.si.ProjectClient.Get(ctx, &project.ProjectQuery{
		Name: projectName,
	})
	argocdSync.TokenMutexProjectMap[projectName].RUnlock()

	if err != nil {
		if strings.Contains(err.Error(), "NotFound") {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.Append(diagnostics.ArgoCDAPIError("read", "project", projectName, err)...)
		return
	}

	// Save updated data into Terraform state
	resp.Diagnostics.Append(resp.State.Set(ctx, newProject(p))...)
}

func (r *projectResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var data projectModel

	// Read Terraform plan data into the model
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)

	// Initialize API clients
	resp.Diagnostics.Append(r.si.InitClients(ctx)...)

	// Check for errors before proceeding
	if resp.Diagnostics.HasError() {
		return
	}

	// Convert model to ArgoCD project
	objectMeta, spec, diags := expandProject(ctx, &data)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	projectName := objectMeta.Name

	// Check feature support
	if !r.si.IsFeatureSupported(features.ProjectSourceNamespaces) && len(data.Spec.SourceNamespaces) > 0 {
		resp.Diagnostics.Append(diagnostics.FeatureNotSupported(features.ProjectSourceNamespaces)...)
		return
	}

	if !r.si.IsFeatureSupported(features.ProjectDestinationServiceAccounts) && len(data.Spec.DestinationServiceAccount) > 0 {
		resp.Diagnostics.Append(diagnostics.FeatureNotSupported(features.ProjectDestinationServiceAccounts)...)
		return
	}

	// Initialize project mutex
	if _, ok := argocdSync.TokenMutexProjectMap[projectName]; !ok {
		argocdSync.TokenMutexProjectMap[projectName] = &sync.RWMutex{}
	}

	argocdSync.TokenMutexProjectMap[projectName].Lock()
	defer argocdSync.TokenMutexProjectMap[projectName].Unlock()

	// Get current project
	p, err := r.si.ProjectClient.Get(ctx, &project.ProjectQuery{
		Name: projectName,
	})
	if err != nil {
		resp.Diagnostics.Append(diagnostics.ArgoCDAPIError("get", "project", projectName, err)...)
		return
	}

	// Preserve preexisting JWTs for managed roles
	roles := expandProjectRoles(ctx, data.Spec.Role)
	for _, r := range roles {
		var pr *v1alpha1.ProjectRole
		var i int

		pr, i, err = p.GetRoleByName(r.Name)
		if err != nil {
			// i == -1 means the role does not exist and was recently added
			if i != -1 {
				resp.Diagnostics.AddError(
					"Project Role Retrieval Failed",
					fmt.Sprintf("project role %s could not be retrieved: %s", r.Name, err.Error()),
				)
				return
			}
		} else {
			// Only preserve preexisting JWTs for managed roles if we found an existing matching project
			spec.Roles[i].JWTTokens = pr.JWTTokens
		}
	}

	// Update project
	projectRequest := &project.ProjectUpdateRequest{
		Project: &v1alpha1.AppProject{
			ObjectMeta: objectMeta,
			Spec:       spec,
		},
	}

	// Kubernetes API requires providing the up-to-date correct ResourceVersion for updates
	projectRequest.Project.ResourceVersion = p.ResourceVersion

	_, err = r.si.ProjectClient.Update(ctx, projectRequest)
	if err != nil {
		resp.Diagnostics.Append(diagnostics.ArgoCDAPIError("update", "project", projectName, err)...)
		return
	}

	tflog.Trace(ctx, fmt.Sprintf("updated project %s", projectName))

	// Read updated resource
	readReq := resource.ReadRequest{State: req.State}
	readResp := resource.ReadResponse{State: resp.State, Diagnostics: resp.Diagnostics}
	r.Read(ctx, readReq, &readResp)
	resp.State = readResp.State
	resp.Diagnostics = readResp.Diagnostics
}

func (r *projectResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data projectModel

	// Read Terraform prior state data into the model
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)

	// Initialize API clients
	resp.Diagnostics.Append(r.si.InitClients(ctx)...)

	// Check for errors before proceeding
	if resp.Diagnostics.HasError() {
		return
	}

	projectName := data.Metadata.Name.ValueString()

	// Initialize project mutex
	if _, ok := argocdSync.TokenMutexProjectMap[projectName]; !ok {
		argocdSync.TokenMutexProjectMap[projectName] = &sync.RWMutex{}
	}

	argocdSync.TokenMutexProjectMap[projectName].Lock()
	_, err := r.si.ProjectClient.Delete(ctx, &project.ProjectQuery{Name: projectName})
	argocdSync.TokenMutexProjectMap[projectName].Unlock()

	if err != nil && !strings.Contains(err.Error(), "NotFound") {
		resp.Diagnostics.Append(diagnostics.ArgoCDAPIError("delete", "project", projectName, err)...)
		return
	}

	tflog.Trace(ctx, fmt.Sprintf("deleted project %s", projectName))
}

func (r *projectResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("metadata").AtName("name"), req, resp)
}

// expandProject converts the Terraform model to ArgoCD API types
func expandProject(ctx context.Context, data *projectModel) (metav1.ObjectMeta, v1alpha1.AppProjectSpec, diag.Diagnostics) {
	var diags diag.Diagnostics

	objectMeta := metav1.ObjectMeta{
		Name:      data.Metadata.Name.ValueString(),
		Namespace: data.Metadata.Namespace.ValueString(),
	}

	if len(data.Metadata.Labels) > 0 {
		labels := make(map[string]string)
		for k, v := range data.Metadata.Labels {
			labels[k] = v.ValueString()
		}
		objectMeta.Labels = labels
	}

	if len(data.Metadata.Annotations) > 0 {
		annotations := make(map[string]string)
		for k, v := range data.Metadata.Annotations {
			annotations[k] = v.ValueString()
		}
		objectMeta.Annotations = annotations
	}

	spec := v1alpha1.AppProjectSpec{}

	if !data.Spec.Description.IsNull() {
		spec.Description = data.Spec.Description.ValueString()
	}

	// Convert source repos
	for _, repo := range data.Spec.SourceRepos {
		spec.SourceRepos = append(spec.SourceRepos, repo.ValueString())
	}

	// Convert signature keys
	for _, key := range data.Spec.SignatureKeys {
		spec.SignatureKeys = append(spec.SignatureKeys, v1alpha1.SignatureKey{KeyID: key.ValueString()})
	}

	// Convert source namespaces
	for _, ns := range data.Spec.SourceNamespaces {
		spec.SourceNamespaces = append(spec.SourceNamespaces, ns.ValueString())
	}

	// Convert destinations
	for _, dest := range data.Spec.Destination {
		d := v1alpha1.ApplicationDestination{
			Namespace: dest.Namespace.ValueString(),
		}
		if !dest.Server.IsNull() {
			d.Server = dest.Server.ValueString()
		}
		if !dest.Name.IsNull() {
			d.Name = dest.Name.ValueString()
		}
		spec.Destinations = append(spec.Destinations, d)
	}

	// Convert destination service accounts
	for _, dsa := range data.Spec.DestinationServiceAccount {
		d := v1alpha1.ApplicationDestinationServiceAccount{
			DefaultServiceAccount: dsa.DefaultServiceAccount.ValueString(),
			Server:                dsa.Server.ValueString(),
		}
		if !dsa.Namespace.IsNull() {
			d.Namespace = dsa.Namespace.ValueString()
		}
		spec.DestinationServiceAccounts = append(spec.DestinationServiceAccounts, d)
	}

	// Convert cluster resource blacklist
	for _, gk := range data.Spec.ClusterResourceBlacklist {
		spec.ClusterResourceBlacklist = append(spec.ClusterResourceBlacklist, metav1.GroupKind{
			Group: gk.Group.ValueString(),
			Kind:  gk.Kind.ValueString(),
		})
	}

	// Convert cluster resource whitelist
	for _, gk := range data.Spec.ClusterResourceWhitelist {
		spec.ClusterResourceWhitelist = append(spec.ClusterResourceWhitelist, metav1.GroupKind{
			Group: gk.Group.ValueString(),
			Kind:  gk.Kind.ValueString(),
		})
	}

	// Convert namespace resource blacklist
	for _, gk := range data.Spec.NamespaceResourceBlacklist {
		spec.NamespaceResourceBlacklist = append(spec.NamespaceResourceBlacklist, metav1.GroupKind{
			Group: gk.Group.ValueString(),
			Kind:  gk.Kind.ValueString(),
		})
	}

	// Convert namespace resource whitelist
	for _, gk := range data.Spec.NamespaceResourceWhitelist {
		spec.NamespaceResourceWhitelist = append(spec.NamespaceResourceWhitelist, metav1.GroupKind{
			Group: gk.Group.ValueString(),
			Kind:  gk.Kind.ValueString(),
		})
	}

	// Convert orphaned resources
	if len(data.Spec.OrphanedResources) > 0 {
		or := data.Spec.OrphanedResources[0]
		spec.OrphanedResources = &v1alpha1.OrphanedResourcesMonitorSettings{}
		if !or.Warn.IsNull() {
			spec.OrphanedResources.Warn = or.Warn.ValueBoolPointer()
		}

		for _, ignore := range or.Ignore {
			i := v1alpha1.OrphanedResourceKey{
				Group: ignore.Group.ValueString(),
				Kind:  ignore.Kind.ValueString(),
			}
			if !ignore.Name.IsNull() {
				i.Name = ignore.Name.ValueString()
			}
			spec.OrphanedResources.Ignore = append(spec.OrphanedResources.Ignore, i)
		}
	}

	// Convert roles
	spec.Roles = expandProjectRoles(ctx, data.Spec.Role)

	// Convert sync windows
	for _, sw := range data.Spec.SyncWindow {
		window := v1alpha1.SyncWindow{}
		if !sw.Duration.IsNull() {
			window.Duration = sw.Duration.ValueString()
		}
		if !sw.Kind.IsNull() {
			window.Kind = sw.Kind.ValueString()
		}
		if !sw.ManualSync.IsNull() {
			window.ManualSync = sw.ManualSync.ValueBool()
		}
		if !sw.Schedule.IsNull() {
			window.Schedule = sw.Schedule.ValueString()
		}
		if !sw.Timezone.IsNull() {
			window.TimeZone = sw.Timezone.ValueString()
		}

		for _, app := range sw.Applications {
			window.Applications = append(window.Applications, app.ValueString())
		}
		for _, cluster := range sw.Clusters {
			window.Clusters = append(window.Clusters, cluster.ValueString())
		}
		for _, ns := range sw.Namespaces {
			window.Namespaces = append(window.Namespaces, ns.ValueString())
		}

		spec.SyncWindows = append(spec.SyncWindows, &window)
	}

	return objectMeta, spec, diags
}

// expandProjectRoles converts project role models to ArgoCD API types
func expandProjectRoles(ctx context.Context, roles []projectRoleModel) []v1alpha1.ProjectRole {
	var result []v1alpha1.ProjectRole

	for _, role := range roles {
		pr := v1alpha1.ProjectRole{
			Name: role.Name.ValueString(),
		}

		if !role.Description.IsNull() {
			pr.Description = role.Description.ValueString()
		}

		for _, policy := range role.Policies {
			pr.Policies = append(pr.Policies, policy.ValueString())
		}

		for _, group := range role.Groups {
			pr.Groups = append(pr.Groups, group.ValueString())
		}

		result = append(result, pr)
	}

	return result
}
