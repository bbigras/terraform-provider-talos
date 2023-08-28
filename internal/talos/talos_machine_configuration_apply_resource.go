// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package talos

import (
	"context"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/retry"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/client"
	"github.com/siderolabs/talos/pkg/machinery/config/configpatcher"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type talosMachineConfigurationApplyResource struct{}

var (
	_ resource.Resource                 = &talosMachineConfigurationApplyResource{}
	_ resource.ResourceWithModifyPlan   = &talosMachineConfigurationApplyResource{}
	_ resource.ResourceWithUpgradeState = &talosMachineConfigurationApplyResource{}
)

type talosMachineConfigurationApplyResourceModelV0 struct {
	Mode                 types.String `tfsdk:"mode"`
	Node                 types.String `tfsdk:"node"`
	Endpoint             types.String `tfsdk:"endpoint"`
	TalosConfig          types.String `tfsdk:"talos_config"`
	MachineConfiguration types.String `tfsdk:"machine_configuration"`
	ConfigPatches        types.List   `tfsdk:"config_patches"`
}

type talosMachineConfigurationApplyResourceModelV1 struct { //nolint:govet
	ID                        types.String        `tfsdk:"id"`
	ApplyMode                 types.String        `tfsdk:"apply_mode"`
	Node                      types.String        `tfsdk:"node"`
	Endpoint                  types.String        `tfsdk:"endpoint"`
	ClientConfiguration       clientConfiguration `tfsdk:"client_configuration"`
	MachineConfigurationInput types.String        `tfsdk:"machine_configuration_input"`
	MachineConfiguration      types.String        `tfsdk:"machine_configuration"`
	ConfigPatches             []types.String      `tfsdk:"config_patches"`
	Timeouts                  timeouts.Value      `tfsdk:"timeouts"`
}

// NewTalosMachineConfigurationApplyResource implements the resource.Resource interface.
func NewTalosMachineConfigurationApplyResource() resource.Resource {
	return &talosMachineConfigurationApplyResource{}
}

func (p *talosMachineConfigurationApplyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_machine_configuration_apply"
}

func (p *talosMachineConfigurationApplyResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Version:     1,
		Description: "The machine configuration apply resource allows to apply machine configuration to a node",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "This is a unique identifier for the machine ",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"apply_mode": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "The mode of the apply operation",
				Validators: []validator.String{
					stringvalidator.OneOf("auto", "reboot", "no_reboot", "staged"),
				},
				Default: stringdefault.StaticString("auto"),
			},
			"node": schema.StringAttribute{
				Required:    true,
				Description: "The name of the node to bootstrap",
			},
			"endpoint": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "The endpoint of the machine to bootstrap",
			},
			"client_configuration": schema.SingleNestedAttribute{
				Attributes: map[string]schema.Attribute{
					"ca_certificate": schema.StringAttribute{
						Required:    true,
						Description: "The client CA certificate",
					},
					"client_certificate": schema.StringAttribute{
						Required:    true,
						Description: "The client certificate",
					},
					"client_key": schema.StringAttribute{
						Required:    true,
						Sensitive:   true,
						Description: "The client key",
					},
				},
				Required:    true,
				Description: "The client configuration data",
			},
			"machine_configuration_input": schema.StringAttribute{
				Description: "The machine configuration to apply",
				Required:    true,
				Sensitive:   true,
			},
			"machine_configuration": schema.StringAttribute{
				Description: "The generated machine configuration after applying patches",
				Computed:    true,
				Sensitive:   true,
			},
			"config_patches": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Description: "The list of config patches to apply",
			},
			"timeouts": timeouts.Attributes(ctx, timeouts.Opts{
				Create: true,
				Update: true,
			}),
		},
	}
}

func (p *talosMachineConfigurationApplyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) { //nolint:dupl
	var state talosMachineConfigurationApplyResourceModelV1

	diags := req.Plan.Get(ctx, &state)
	resp.Diagnostics.Append(diags...)

	if diags.HasError() {
		return
	}

	talosClientConfig, err := talosClientTFConfigToTalosClientConfig(
		"dynamic",
		state.ClientConfiguration.CA.ValueString(),
		state.ClientConfiguration.Cert.ValueString(),
		state.ClientConfiguration.Key.ValueString(),
	)
	if err != nil {
		resp.Diagnostics.AddError(
			"Error converting config to talos client config",
			err.Error(),
		)

		return
	}

	createTimeout, diags := state.Timeouts.Create(ctx, 10*time.Minute)
	resp.Diagnostics.Append(diags...)

	if resp.Diagnostics.HasError() {
		return
	}

	ctxDeadline, cancel := context.WithTimeout(ctx, createTimeout)
	defer cancel()

	if err := retry.RetryContext(ctxDeadline, createTimeout, func() *retry.RetryError {
		if err := talosClientOp(ctx, state.Endpoint.ValueString(), state.Node.ValueString(), talosClientConfig, func(nodeCtx context.Context, c *client.Client) error {
			_, err := c.ApplyConfiguration(nodeCtx, &machineapi.ApplyConfigurationRequest{
				Mode: machineapi.ApplyConfigurationRequest_Mode(machineapi.ApplyConfigurationRequest_Mode_value[strings.ToUpper(state.ApplyMode.ValueString())]),
				Data: []byte(state.MachineConfiguration.ValueString()),
			})
			if err != nil {
				return err
			}

			return nil
		}); err != nil {
			if s := status.Code(err); s == codes.InvalidArgument {
				return retry.NonRetryableError(err)
			}

			return retry.RetryableError(err)
		}

		return nil
	}); err != nil {
		resp.Diagnostics.AddError(
			"Error applying configuration",
			err.Error(),
		)

		return
	}

	state.ID = basetypes.NewStringValue("machine_configuration_apply")

	// Set state to fully populated data
	diags = resp.State.Set(ctx, &state)
	resp.Diagnostics.Append(diags...)

	if resp.Diagnostics.HasError() {
		return
	}
}

func (p *talosMachineConfigurationApplyResource) Read(_ context.Context, _ resource.ReadRequest, _ *resource.ReadResponse) {
}

func (p *talosMachineConfigurationApplyResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) { //nolint:dupl
	var state talosMachineConfigurationApplyResourceModelV1

	diags := req.Plan.Get(ctx, &state)
	resp.Diagnostics.Append(diags...)

	if diags.HasError() {
		return
	}

	talosClientConfig, err := talosClientTFConfigToTalosClientConfig(
		"dynamic",
		state.ClientConfiguration.CA.ValueString(),
		state.ClientConfiguration.Cert.ValueString(),
		state.ClientConfiguration.Key.ValueString(),
	)
	if err != nil {
		resp.Diagnostics.AddError(
			"Error converting config to talos client config",
			err.Error(),
		)

		return
	}

	updateTimeout, diags := state.Timeouts.Update(ctx, 10*time.Minute)
	resp.Diagnostics.Append(diags...)

	if resp.Diagnostics.HasError() {
		return
	}

	ctxDeadline, cancel := context.WithTimeout(ctx, updateTimeout)
	defer cancel()

	if err := retry.RetryContext(ctxDeadline, updateTimeout, func() *retry.RetryError {
		if err := talosClientOp(ctx, state.Endpoint.ValueString(), state.Node.ValueString(), talosClientConfig, func(nodeCtx context.Context, c *client.Client) error {
			_, err := c.ApplyConfiguration(nodeCtx, &machineapi.ApplyConfigurationRequest{
				Mode: machineapi.ApplyConfigurationRequest_Mode(machineapi.ApplyConfigurationRequest_Mode_value[strings.ToUpper(state.ApplyMode.ValueString())]),
				Data: []byte(state.MachineConfiguration.ValueString()),
			})
			if err != nil {
				return err
			}

			return nil
		}); err != nil {
			if s := status.Code(err); s == codes.InvalidArgument {
				return retry.NonRetryableError(err)
			}

			return retry.RetryableError(err)
		}

		return nil
	}); err != nil {
		resp.Diagnostics.AddError(
			"Error applying configuration",
			err.Error(),
		)

		return
	}

	state.ID = basetypes.NewStringValue("machine_configuration_apply")

	// Set state to fully populated data
	diags = resp.State.Set(ctx, &state)
	resp.Diagnostics.Append(diags...)

	if resp.Diagnostics.HasError() {
		return
	}
}

func (p *talosMachineConfigurationApplyResource) Delete(_ context.Context, _ resource.DeleteRequest, _ *resource.DeleteResponse) {
}

func (p *talosMachineConfigurationApplyResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) { //nolint:gocyclo,cyclop
	// delete is a no-op
	if req.Plan.Raw.IsNull() {
		return
	}

	var configObj types.Object

	diags := req.Config.Get(ctx, &configObj)
	resp.Diagnostics.Append(diags...)

	if resp.Diagnostics.HasError() {
		return
	}

	var config talosMachineConfigurationApplyResourceModelV1

	diags = configObj.As(ctx, &config, basetypes.ObjectAsOptions{
		UnhandledNullAsEmpty:    true,
		UnhandledUnknownAsEmpty: true,
	})
	resp.Diagnostics.Append(diags...)

	if resp.Diagnostics.HasError() {
		return
	}

	// if either endpoint or node is unknown return early
	if config.Endpoint.IsUnknown() || config.Node.IsUnknown() || config.MachineConfiguration.IsUnknown() {
		return
	}

	var planObj types.Object

	diags = req.Plan.Get(ctx, &planObj)
	resp.Diagnostics.Append(diags...)

	if resp.Diagnostics.HasError() {
		return
	}

	var planState talosMachineConfigurationApplyResourceModelV1

	diags = configObj.As(ctx, &planState, basetypes.ObjectAsOptions{
		UnhandledNullAsEmpty:    true,
		UnhandledUnknownAsEmpty: true,
	})
	resp.Diagnostics.Append(diags...)

	if resp.Diagnostics.HasError() {
		return
	}

	if planState.Endpoint.IsUnknown() || planState.Endpoint.IsNull() {
		diags = resp.Plan.SetAttribute(ctx, path.Root("endpoint"), planState.Node.ValueString())
		resp.Diagnostics.Append(diags...)

		if diags.HasError() {
			return
		}
	}

	if !planState.MachineConfigurationInput.IsUnknown() && !planState.MachineConfigurationInput.IsNull() {
		configPatches := make([]string, len(planState.ConfigPatches))

		for i, patch := range planState.ConfigPatches {
			// if any of the patches is unknown, return early
			if patch.IsUnknown() {
				return
			}

			if !patch.IsUnknown() && !patch.IsNull() {
				configPatches[i] = patch.ValueString()
			}
		}

		patches, err := configpatcher.LoadPatches(configPatches)
		if err != nil {
			resp.Diagnostics.AddError(
				"Error loading config patches",
				err.Error(),
			)

			return
		}

		cfg, err := configpatcher.Apply(configpatcher.WithBytes([]byte(planState.MachineConfigurationInput.ValueString())), patches)
		if err != nil {
			resp.Diagnostics.AddError(
				"Error applying config patches",
				err.Error(),
			)

			return
		}

		cfgBytes, err := cfg.Bytes()
		if err != nil {
			resp.Diagnostics.AddError(
				"Error converting config to bytes",
				err.Error(),
			)

			return
		}

		diags = resp.Plan.SetAttribute(ctx, path.Root("machine_configuration"), string(cfgBytes))
		resp.Diagnostics.Append(diags...)

		if diags.HasError() {
			return
		}
	}
}

func (p *talosMachineConfigurationApplyResource) UpgradeState(_ context.Context) map[int64]resource.StateUpgrader {
	return map[int64]resource.StateUpgrader{
		0: {
			PriorSchema: &schema.Schema{
				Attributes: map[string]schema.Attribute{
					"mode": schema.StringAttribute{
						Optional: true,
					},
					"endpoint": schema.StringAttribute{
						Required: true,
					},
					"node": schema.StringAttribute{
						Required: true,
					},
					"talos_config": schema.StringAttribute{
						Required: true,
					},
					"machine_configuration": schema.StringAttribute{
						Required: true,
					},
					"config_patches": schema.ListAttribute{
						Optional:    true,
						ElementType: types.StringType,
					},
				},
			},
			StateUpgrader: func(ctx context.Context, req resource.UpgradeStateRequest, resp *resource.UpgradeStateResponse) {
				var priorStateData talosMachineConfigurationApplyResourceModelV0

				diags := req.State.Get(ctx, &priorStateData)
				resp.Diagnostics.Append(diags...)
				if diags.HasError() {
					return
				}

				var patches []string
				diags = append(diags, priorStateData.ConfigPatches.ElementsAs(ctx, &patches, true)...)
				resp.Diagnostics.Append(diags...)
				if diags.HasError() {
					return
				}

				configPatches := make([]basetypes.StringValue, len(patches))
				for i, patch := range patches {
					configPatches[i] = basetypes.NewStringValue(patch)
				}

				timeout, diag := basetypes.NewObjectValue(map[string]attr.Type{
					"create": types.StringType,
					"update": types.StringType,
				}, map[string]attr.Value{
					"create": basetypes.NewStringNull(),
					"update": basetypes.NewStringNull(),
				})
				resp.Diagnostics.Append(diag...)
				if resp.Diagnostics.HasError() {
					return
				}

				state := talosMachineConfigurationApplyResourceModelV1{
					ID:                        basetypes.NewStringValue("machine_configuration_apply"),
					ApplyMode:                 priorStateData.Mode,
					Node:                      priorStateData.Node,
					Endpoint:                  priorStateData.Endpoint,
					MachineConfigurationInput: priorStateData.MachineConfiguration,
					ConfigPatches:             configPatches,
					Timeouts: timeouts.Value{
						Object: timeout,
					},
				}

				// Set state to fully populated data
				diags = resp.State.Set(ctx, state)
				resp.Diagnostics.Append(diags...)
				if resp.Diagnostics.HasError() {
					return
				}
			},
		},
	}
}
