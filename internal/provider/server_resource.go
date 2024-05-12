// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"errors"
	"fmt"
	"github.com/cenkalti/backoff/v4"
	"github.com/cherryservers/cherrygo/v3"
	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	"golang.org/x/exp/slices"
	"strconv"
	"time"
)

// Ensure provider defined types fully satisfy framework interfaces.
var (
	_ resource.Resource                = &serverResource{}
	_ resource.ResourceWithConfigure   = &serverResource{}
	_ resource.ResourceWithImportState = &serverResource{}
	_ planmodifier.String              = warnServerReinstallNeededModifier{}
	_ planmodifier.Int64               = warnServerReinstallNeededModifier{}
	_ planmodifier.Set                 = warnServerReinstallNeededModifier{}
)

func NewServerResource() resource.Resource {
	return &serverResource{}
}

// serverResource defines the resource implementation.
type serverResource struct {
	client *cherrygo.Client
}

// serverResourceModel describes the resource data model.
type serverResourceModel struct {
	Plan                types.String   `tfsdk:"plan"`
	ProjectId           types.Int64    `tfsdk:"project_id"`
	Region              types.String   `tfsdk:"region"`
	Hostname            types.String   `tfsdk:"hostname"`
	Name                types.String   `tfsdk:"name"`
	Image               types.String   `tfsdk:"image"`
	SSHKeyIds           types.Set      `tfsdk:"ssh_key_ids"`
	ExtraIPAddressesIds types.Set      `tfsdk:"extra_ip_addresses_ids"`
	UserData            types.String   `tfsdk:"user_data"`
	Tags                types.Map      `tfsdk:"tags"`
	SpotInstance        types.Bool     `tfsdk:"spot_instance"`
	OSPartitionSize     types.Int64    `tfsdk:"os_partition_size"`
	PowerState          types.String   `tfsdk:"power_state"`
	State               types.String   `tfsdk:"state"`
	IpAddresses         types.Set      `tfsdk:"ip_addresses"`
	Id                  types.String   `tfsdk:"id"`
	Timeouts            timeouts.Value `tfsdk:"timeouts"`
}

func (d *serverResourceModel) populateState(server cherrygo.Server, ctx context.Context, diags diag.Diagnostics, powerState string) {
	d.Plan = types.StringValue(server.Plan.Slug)
	d.ProjectId = types.Int64Value(int64(server.Project.ID))
	d.Region = types.StringValue(server.Region.Slug)
	d.Hostname = types.StringValue(server.Hostname)
	d.Name = types.StringValue(server.Name)
	d.Image = types.StringValue(server.Image)

	var sshKeyIds, ipIds []string
	for _, sshKey := range server.SSHKeys {
		sshKeyID := strconv.Itoa(sshKey.ID)
		sshKeyIds = append(sshKeyIds, sshKeyID)
	}
	sshKeyIdsTf, sshDiags := types.SetValueFrom(ctx, types.StringType, sshKeyIds)
	d.SSHKeyIds = sshKeyIdsTf
	diags.Append(sshDiags...)

	var ips []attr.Value
	for _, ip := range server.IPAddresses {

		// ExtraIPAddresses shouldn't have unmodifiable (primary and private type) IPs
		if ip.Type == "subnet" || ip.Type == "floating-ip" {
			ipIds = append(ipIds, ip.ID)
		}

		ipModel := ipAddressFlatResourceModel{
			Id:            types.StringValue(ip.ID),
			Type:          types.StringValue(ip.Type),
			Address:       types.StringValue(ip.Address),
			AddressFamily: types.Int64Value(int64(ip.AddressFamily)),
			CIDR:          types.StringValue(ip.Cidr),
		}

		ipTf, ipDiags := types.ObjectValueFrom(ctx, ipModel.AttributeTypes(), ipModel)
		diags.Append(ipDiags...)

		ips = append(ips, ipTf)
	}

	ipsTf, ipsDiags := types.SetValue(types.ObjectType{AttrTypes: ipAddressFlatResourceModel{}.AttributeTypes()}, ips)
	diags.Append(ipsDiags...)
	d.IpAddresses = ipsTf

	ipIdsTf, ipIdDiags := types.SetValueFrom(ctx, types.StringType, ipIds)
	d.ExtraIPAddressesIds = ipIdsTf
	diags.Append(ipIdDiags...)

	tags, tagsDiags := types.MapValueFrom(ctx, types.StringType, server.Tags)
	d.Tags = tags
	diags.Append(tagsDiags...)

	d.SpotInstance = types.BoolValue(server.SpotInstance)
	d.PowerState = types.StringValue(powerState)
	d.State = types.StringValue(server.State)
	d.Id = types.StringValue(strconv.Itoa(server.ID))
}

type ipAddressFlatResourceModel struct {
	Id            types.String `tfsdk:"id"`
	Type          types.String `tfsdk:"type"`
	Address       types.String `tfsdk:"address"`
	AddressFamily types.Int64  `tfsdk:"address_family"`
	CIDR          types.String `tfsdk:"cidr"`
}

func (m ipAddressFlatResourceModel) AttributeTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"id":             types.StringType,
		"type":           types.StringType,
		"address":        types.StringType,
		"address_family": types.Int64Type,
		"cidr":           types.StringType,
	}
}

type warnServerReinstallNeededModifier struct {
}

func (m warnServerReinstallNeededModifier) Description(_ context.Context) string {
	return "Diagnostics warning that a server reinstall will be needed"
}

func (m warnServerReinstallNeededModifier) MarkdownDescription(_ context.Context) string {
	return "Diagnostics warning that a server reinstall will be needed"
}

func (m warnServerReinstallNeededModifier) PlanModifyString(_ context.Context, req planmodifier.StringRequest, resp *planmodifier.StringResponse) {
	// Not applicable on resource creation and destruction
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}

	if req.PlanValue.IsUnknown() {
		return
	}

	if req.StateValue.Equal(req.PlanValue) {
		return
	}

	resp.Diagnostics.AddAttributeWarning(req.Path, "Warning: server reinstall required",
		`When updating "image", "password", "ssh_key_ids", "os_partition_size" or "user_data" values, the server OS has to be reinstalled.`)
}

func WarnServerReinstallNeededString() planmodifier.String {
	return warnServerReinstallNeededModifier{}
}

func (m warnServerReinstallNeededModifier) PlanModifyInt64(_ context.Context, req planmodifier.Int64Request, resp *planmodifier.Int64Response) {
	// Not applicable on resource creation and destruction
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}

	if req.PlanValue.IsUnknown() {
		return
	}

	if req.StateValue.Equal(req.PlanValue) {
		return
	}

	resp.Diagnostics.AddAttributeWarning(req.Path, "Warning: server reinstall required",
		`When updating "image", "password", "ssh_key_ids", "os_partition_size" or "user_data" values, the server OS has to be reinstalled.`)
}

func WarnServerReinstallNeededInt64() planmodifier.Int64 {
	return warnServerReinstallNeededModifier{}
}

func (m warnServerReinstallNeededModifier) PlanModifySet(_ context.Context, req planmodifier.SetRequest, resp *planmodifier.SetResponse) {
	// Not applicable on resource creation and destruction
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}

	if req.PlanValue.IsUnknown() {
		return
	}

	if req.StateValue.Equal(req.PlanValue) {
		return
	}

	resp.Diagnostics.AddAttributeWarning(req.Path, "Warning: server reinstall required",
		`When updating "image", "password", "ssh_key_ids", "os_partition_size" or "user_data" values, the server OS has to be reinstalled.`)
}

func WarnServerReinstallNeededSet() planmodifier.Set {
	return warnServerReinstallNeededModifier{}
}

func (r *serverResource) Metadata(ctx context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_server"
}

func (r *serverResource) Schema(ctx context.Context, req resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		// This description is used by the documentation generator and the language server.
		Description: "Provides a Cherry Servers server resource. This can be used to create, read, modify, and delete servers on your Cherry Servers account.",

		Attributes: map[string]schema.Attribute{
			"plan": schema.StringAttribute{
				Required: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
				Description: "Slug of the plan. Example: e5_1620v4. [See List Plans](https://api.cherryservers.com/doc/#tag/Plans/operation/get-plans)",
			},
			"project_id": schema.Int64Attribute{
				Description: "CherryServers project id, associated with the server",
				Required:    true,
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.RequiresReplace(),
				},
			},
			"region": schema.StringAttribute{
				Description: "Slug of the region. Example: eu_nord_1 [See List Regions](https://api.cherryservers.com/doc/#tag/Regions/operation/get-regions)",
				Required:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"name": schema.StringAttribute{
				Description: "Name of the server",
				Optional:    true,
				Computed:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"hostname": schema.StringAttribute{
				Description: "Hostname of the server",
				Optional:    true,
				Computed:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"image": schema.StringAttribute{
				Description: "Slug of the operating system. Example: ubuntu_22_04. [See List Images](https://api.cherryservers.com/doc/#tag/Images/operation/get-plan-images)",
				Optional:    true,
				Computed:    true,
				PlanModifiers: []planmodifier.String{
					WarnServerReinstallNeededString(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"ssh_key_ids": schema.SetAttribute{
				Description: "List of the SSH key IDs allowed to SSH to the server",
				Optional:    true,
				Computed:    true,
				ElementType: types.StringType,
				Default:     setdefault.StaticValue(types.SetNull(types.StringType)),
				PlanModifiers: []planmodifier.Set{
					WarnServerReinstallNeededSet(),
					setplanmodifier.UseStateForUnknown(),
				},
			},
			"extra_ip_addresses_ids": schema.SetAttribute{
				Description: "List of the IP address IDs to be embedded into the Server",
				Optional:    true,
				Computed:    true,
				ElementType: types.StringType,
				Default:     setdefault.StaticValue(types.SetNull(types.StringType)),
				PlanModifiers: []planmodifier.Set{
					setplanmodifier.UseStateForUnknown(),
				},
			},
			"user_data": schema.StringAttribute{
				Description: "Base64 encoded User-Data blob. It should be either a bash or cloud-config script",
				Optional:    true,
				PlanModifiers: []planmodifier.String{
					WarnServerReinstallNeededString(),
				},
			},
			"tags": schema.MapAttribute{
				Description: "Key/value metadata for server tagging",
				Optional:    true,
				ElementType: types.StringType,
				Default:     mapdefault.StaticValue(types.MapValueMust(types.StringType, map[string]attr.Value{})),
				Computed:    true,
				PlanModifiers: []planmodifier.Map{
					mapplanmodifier.UseStateForUnknown(),
				},
			},
			"spot_instance": schema.BoolAttribute{
				Description: "If True, provisions the server as a spot instance",
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(false),
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.RequiresReplace(),
					boolplanmodifier.UseStateForUnknown(),
				},
			},
			"os_partition_size": schema.Int64Attribute{
				Description: "OS partition size in GB",
				Optional:    true,
				PlanModifiers: []planmodifier.Int64{
					WarnServerReinstallNeededInt64(),
					int64planmodifier.UseStateForUnknown(),
				},
			},
			"power_state": schema.StringAttribute{
				Description: "The power state of the server, such as 'Powered off' or 'Powered on'",
				Computed:    true,
			},
			"state": schema.StringAttribute{
				Description: "The state of the server, such as 'pending' or 'active'",
				Computed:    true,
			},
			"ip_addresses": schema.SetNestedAttribute{
				Description: "IP addresses attached to the server",
				PlanModifiers: []planmodifier.Set{
					setplanmodifier.UseStateForUnknown(),
					//TODO Use state for unknown if no extra addresses
				},
				Computed: true,
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"id": schema.StringAttribute{
							Description: "ID of the IP address",
							Computed:    true,
						},
						"type": schema.StringAttribute{
							Description: "Type of the IP address",
							Computed:    true,
						},
						"address": schema.StringAttribute{
							Description: "Address of the IP address",
							Computed:    true,
						},
						"address_family": schema.Int64Attribute{
							Description: "Address family of the IP address",
							Computed:    true,
						},
						"cidr": schema.StringAttribute{
							Description: "CIDR of the IP address",
							Computed:    true,
						},
					},
				},
			},
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Server identifier",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"timeouts": timeouts.Attributes(ctx, timeouts.Opts{
				Create: true,
				Update: true,
			}),
		},
	}
}

func (r *serverResource) Configure(ctx context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	// Prevent panic if the provider has not been configured.
	if req.ProviderData == nil {
		return
	}

	client, ok := req.ProviderData.(*cherrygo.Client)

	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Resource Configure Type",
			fmt.Sprintf("Expected *cherrygo.Client, got: %T. Please report this issue to the provider developers.", req.ProviderData),
		)

		return
	}

	r.client = client
}

func (r *serverResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data serverResourceModel

	// Read Terraform plan data into the model
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)

	if resp.Diagnostics.HasError() {
		return
	}

	request := &cherrygo.CreateServer{
		ProjectID:    int(data.ProjectId.ValueInt64()),
		Plan:         data.Plan.ValueString(),
		Region:       data.Region.ValueString(),
		Image:        data.Image.ValueString(),
		Hostname:     data.Hostname.ValueString(),
		SpotInstance: data.SpotInstance.ValueBool(),
	}

	sshIds := make([]string, len(data.SSHKeyIds.Elements()))
	diags := data.SSHKeyIds.ElementsAs(ctx, &sshIds, false)
	resp.Diagnostics.Append(diags...)

	request.SSHKeys = sshIds

	ipsIds := make([]string, len(data.ExtraIPAddressesIds.Elements()))
	diags = data.ExtraIPAddressesIds.ElementsAs(ctx, &ipsIds, false)
	resp.Diagnostics.Append(diags...)

	request.IPAddresses = ipsIds

	tagsMap := make(map[string]string, len(data.Tags.Elements()))
	diags = data.Tags.ElementsAs(ctx, &tagsMap, false)
	resp.Diagnostics.Append(diags...)

	request.Tags = &tagsMap

	if !data.UserData.IsNull() {
		if !IsBase64(data.UserData.ValueString()) {
			resp.Diagnostics.AddError("invalid UserData", "error creating server, user_data property must be base64 encoded value")
			return
		}
		request.UserData = data.UserData.ValueString()
	}

	if !data.OSPartitionSize.IsNull() {
		request.OSPartitionSize = int(data.OSPartitionSize.ValueInt64())
	}

	server, _, err := r.client.Servers.Create(request)
	if err != nil {
		resp.Diagnostics.AddError(
			"unable to create a CherryServers server resource",
			err.Error(),
		)
		return
	}

	createTimeout, diags := data.Timeouts.Create(ctx, 60*time.Minute)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	err = backoff.Retry(
		func() error {
			stateOption := cherrygo.GetOptions{Fields: []string{"state"}}
			s, _, e := r.client.Servers.Get(server.ID, &stateOption)
			if e != nil {
				return backoff.Permanent(e)
			}

			if s.State == "pending" || s.State == "provisioning" {
				return errors.New("server is in inactive state")
			}

			if s.State == "active" {
				return nil
			}

			return backoff.Permanent(errors.New("failed to deploy server"))

		}, backoff.NewExponentialBackOff(
			backoff.WithMaxElapsedTime(createTimeout),
			backoff.WithInitialInterval(time.Second*10)))
	if err != nil {
		resp.Diagnostics.AddError("unable to deploy CherryServers server", err.Error())
		return
	}

	// If applicable, this is a great opportunity to initialize any necessary
	// provider client data and make a call using it.
	// httpResp, err := r.client.Do(httpReq)
	// if err != nil {
	//     resp.Diagnostics.AddError("Client Error", fmt.Sprintf("Unable to create server, got error: %s", err))
	//     return
	// }

	powerState, _, err := r.client.Servers.PowerState(server.ID)
	if err != nil {
		resp.Diagnostics.AddError("unable to get CherryServers server power-state", err.Error())
		return
	}

	//Workaround for not being able to set BGP and Name on "Request a server" request in API
	// TODO: add BGP
	updateRequest := cherrygo.UpdateServer{
		Name: data.Name.ValueString(),
	}

	server, _, err = r.client.Servers.Update(server.ID, &updateRequest)
	if err != nil {
		resp.Diagnostics.AddError("unable to update a CherryServers server resource with name/bgp after it's creation", err.Error())
		return
	}

	data.populateState(server, ctx, resp.Diagnostics, powerState.Power)
	// For the purposes of this server code, hardcoding a response value to
	// save into the Terraform state.

	// Write logs using the tflog package
	// Documentation: https://terraform.io/plugin/log
	tflog.SetField(ctx, "server_id", data.Id)
	tflog.Trace(ctx, "created a resource")

	// Save data into Terraform state
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *serverResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data serverResourceModel

	// Read Terraform prior state data into the model
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)

	if resp.Diagnostics.HasError() {
		return
	}

	serverID, err := strconv.Atoi(data.Id.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("invalid server ID in state", err.Error())
		return
	}
	server, serverGetResp, err := r.client.Servers.Get(serverID, nil)
	if err != nil {
		if is404Error(serverGetResp) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError(
			"unable to read a CherryServers server resource",
			err.Error(),
		)
		return
	}

	if server.State == "terminating" {
		resp.State.RemoveResource(ctx)
		return
	}

	powerState, _, err := r.client.Servers.PowerState(server.ID)
	if err != nil {
		resp.Diagnostics.AddError("unable to get CherryServers server power-state", err.Error())
		return
	}

	data.populateState(server, ctx, resp.Diagnostics, powerState.Power)

	// If applicable, this is a great opportunity to initialize any necessary
	// provider client data and make a call using it.
	// httpResp, err := r.client.Do(httpReq)
	// if err != nil {
	//     resp.Diagnostics.AddError("Client Error", fmt.Sprintf("Unable to read server, got error: %s", err))
	//     return
	// }

	// Save updated data into Terraform state
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *serverResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	//TODO//
	var plan, state serverResourceModel

	// Read Terraform plan and state data into the model
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)

	if resp.Diagnostics.HasError() {
		return
	}

	serverID, _ := strconv.Atoi(plan.Id.ValueString())

	requestReinstall := cherrygo.ReinstallServerFields{}
	reinstallNeeded := false
	if !plan.Image.Equal(state.Image) {
		requestReinstall.Image = plan.Image.ValueString()
		reinstallNeeded = true
	}

	if !plan.SSHKeyIds.Equal(state.SSHKeyIds) {
		sshIds := make([]string, len(plan.SSHKeyIds.Elements()))
		diags := plan.SSHKeyIds.ElementsAs(ctx, &sshIds, false)
		resp.Diagnostics.Append(diags...)

		requestReinstall.SSHKeys = sshIds
		reinstallNeeded = true
	}

	if !plan.OSPartitionSize.Equal(state.OSPartitionSize) {
		requestReinstall.OSPartitionSize = int(plan.OSPartitionSize.ValueInt64())
		reinstallNeeded = true
	}

	if !plan.UserData.Equal(state.UserData) {
		if !IsBase64(plan.UserData.ValueString()) {
			resp.Diagnostics.AddError("invalid UserData", "error reinstalling server, user_data property must be base64 encoded value")
			return
		}
		reinstallNeeded = true
	}

	if reinstallNeeded {
		_, _, err := r.client.Servers.Reinstall(serverID, &requestReinstall)
		if err != nil {
			resp.Diagnostics.AddError("unable to reinstall a CherryServers server resource", err.Error())
		}
		return
	}

	if !plan.ExtraIPAddressesIds.Equal(state.ExtraIPAddressesIds) {
		for _, ip := range plan.ExtraIPAddressesIds.Elements() {
			if !slices.Contains(state.ExtraIPAddressesIds.Elements(), ip) {
				ipRequest := cherrygo.UpdateIPAddress{
					TargetedTo: plan.Id.ValueString(),
				}
				ipTf, err := ip.ToTerraformValue(ctx)
				if err != nil {
					resp.Diagnostics.AddError("invalid IP value in plan", err.Error())
					return
				}
				if ipTf.IsKnown() {
					var ipStr string
					_ = ipTf.As(&ipStr)
					_, _, err = r.client.IPAddresses.Update(ipStr, &ipRequest)
					if err != nil {
						resp.Diagnostics.AddError("unable to update IP address in CherryServers server update operation", err.Error())
					}
				}
			}
		}
	}

	requestUpdate := cherrygo.UpdateServer{
		Hostname: plan.Hostname.ValueString(),
		Name:     plan.Name.ValueString(),
	}

	tagsMap := make(map[string]string, len(plan.Tags.Elements()))
	diags := plan.Tags.ElementsAs(ctx, &tagsMap, false)
	resp.Diagnostics.Append(diags...)

	requestUpdate.Tags = &tagsMap

	server, _, err := r.client.Servers.Update(serverID, &requestUpdate)
	if err != nil {
		resp.Diagnostics.AddError(
			"unable to update a CherryServers server resource",
			err.Error(),
		)
		return
	}

	// If applicable, this is a great opportunity to initialize any necessary
	// provider client plan and make a call using it.
	// httpResp, err := r.client.Do(httpReq)
	// if err != nil {
	//     resp.Diagnostics.AddError("Client Error", fmt.Sprintf("Unable to update server, got error: %s", err))
	//     return
	// }
	powerState, _, err := r.client.Servers.PowerState(server.ID)
	if err != nil {
		resp.Diagnostics.AddError("unable to get CherryServers server power-state", err.Error())
		return
	}

	plan.populateState(server, ctx, resp.Diagnostics, powerState.Power)

	// Save updated plan into Terraform state
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *serverResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data serverResourceModel

	// Read Terraform prior state data into the model
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)

	if resp.Diagnostics.HasError() {
		return
	}

	serverID, _ := strconv.Atoi(data.Id.ValueString())

	if _, _, err := r.client.Servers.Delete(serverID); err != nil {
		resp.Diagnostics.AddError(
			"unable to delete a CherryServers server resource",
			err.Error(),
		)
		return
	}

	ctx = tflog.SetField(ctx, "server_id", data.Id)
	tflog.Trace(ctx, "deleted a resource")

	// If applicable, this is a great opportunity to initialize any necessary
	// provider client data and make a call using it.
	// httpResp, err := r.client.Do(httpReq)
	// if err != nil {
	//     resp.Diagnostics.AddError("Client Error", fmt.Sprintf("Unable to delete server, got error: %s", err))
	//     return
	// }
}

func (r *serverResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
