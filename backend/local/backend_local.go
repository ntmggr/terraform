package local

import (
	"context"
	"fmt"

	"github.com/hashicorp/errwrap"

	"github.com/hashicorp/terraform/backend"
	"github.com/hashicorp/terraform/command/clistate"
	"github.com/hashicorp/terraform/configs/configload"
	"github.com/hashicorp/terraform/plans/planfile"
	"github.com/hashicorp/terraform/state"
	"github.com/hashicorp/terraform/terraform"
	"github.com/hashicorp/terraform/tfdiags"
)

// backend.Local implementation.
func (b *Local) Context(op *backend.Operation) (*terraform.Context, state.State, tfdiags.Diagnostics) {
	// Make sure the type is invalid. We use this as a way to know not
	// to ask for input/validate.
	op.Type = backend.OperationTypeInvalid

	if op.LockState {
		op.StateLocker = clistate.NewLocker(context.Background(), op.StateLockTimeout, b.CLI, b.Colorize())
	} else {
		op.StateLocker = clistate.NewNoopLocker()
	}

	return b.context(op)
}

func (b *Local) context(op *backend.Operation) (*terraform.Context, state.State, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics

	// Get the latest state.
	s, err := b.State(op.Workspace)
	if err != nil {
		diags = diags.Append(errwrap.Wrapf("Error loading state: {{err}}", err))
		return nil, nil, diags
	}
	if err := op.StateLocker.Lock(s, op.Type.String()); err != nil {
		diags = diags.Append(errwrap.Wrapf("Error locking state: {{err}}", err))
		return nil, nil, diags
	}
	if err := s.RefreshState(); err != nil {
		diags = diags.Append(errwrap.Wrapf("Error loading state: {{err}}", err))
		return nil, nil, diags
	}

	// Initialize our context options
	var opts terraform.ContextOpts
	if v := b.ContextOpts; v != nil {
		opts = *v
	}

	// Copy set options from the operation
	opts.Destroy = op.Destroy
	opts.Targets = op.Targets
	opts.UIInput = op.UIIn

	// Load the latest state. If we enter contextFromPlanFile below then the
	// state snapshot in the plan file must match this.
	opts.State = s.State()

	var tfCtx *terraform.Context
	var ctxDiags tfdiags.Diagnostics
	if op.PlanFile != nil {
		var configSources map[string][]byte
		tfCtx, configSources, ctxDiags = b.contextFromPlanFile(op.PlanFile, opts)
		// Write sources into the cache of the main loader so that they are
		// available if we need to generate diagnostic message snippets.
		op.ConfigLoader.ImportSources(configSources)
	} else {
		tfCtx, ctxDiags = b.contextDirect(op, opts)
	}
	diags = diags.Append(ctxDiags)

	// If we have an operation, then we automatically do the input/validate
	// here since every option requires this.
	if op.Type != backend.OperationTypeInvalid {
		// If input asking is enabled, then do that
		if op.PlanFile == nil && b.OpInput {
			mode := terraform.InputModeProvider
			mode |= terraform.InputModeVar
			mode |= terraform.InputModeVarUnset

			inputDiags := tfCtx.Input(mode)
			diags = diags.Append(inputDiags)
			if inputDiags.HasErrors() {
				return nil, nil, diags
			}
		}

		// If validation is enabled, validate
		if b.OpValidation {
			validateDiags := tfCtx.Validate()
			diags = diags.Append(validateDiags)
		}
	}

	return tfCtx, s, diags
}

func (b *Local) contextDirect(op *backend.Operation, opts terraform.ContextOpts) (*terraform.Context, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics

	// Load the configuration using the caller-provided configuration loader.
	config, configDiags := op.ConfigLoader.LoadConfig(op.ConfigDir)
	diags = diags.Append(configDiags)
	if configDiags.HasErrors() {
		return nil, diags
	}
	opts.Config = config

	variables, varDiags := backend.ParseVariableValues(op.Variables, config.Module.Variables)
	diags = diags.Append(varDiags)
	if diags.HasErrors() {
		return nil, diags
	}
	if op.Variables != nil {
		opts.Variables = variables
	}

	tfCtx, ctxDiags := terraform.NewContext(&opts)
	diags = diags.Append(ctxDiags)
	return tfCtx, diags
}

func (b *Local) contextFromPlanFile(pf *planfile.Reader, opts terraform.ContextOpts) (*terraform.Context, map[string][]byte, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics

	const errSummary = "Invalid plan file"

	// A plan file has a snapshot of configuration embedded inside it, which
	// is used instead of whatever configuration might be already present
	// in the filesystem.
	snap, err := pf.ReadConfigSnapshot()
	if err != nil {
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			errSummary,
			fmt.Sprintf("Failed to read configuration snapshot from plan file: %s.", err),
		))
	}
	loader := configload.NewLoaderFromSnapshot(snap)
	config, configDiags := loader.LoadConfig(snap.Modules[""].Dir)
	diags = diags.Append(configDiags)
	sources := loader.Sources()
	if configDiags.HasErrors() {
		return nil, sources, diags
	}
	opts.Config = config

	plan, err := pf.ReadPlan()
	if err != nil {
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			errSummary,
			fmt.Sprintf("Failed to read plan from plan file: %s.", err),
		))
	}

	variables := terraform.InputValues{}
	for name, dyVal := range plan.VariableValues {
		ty, err := dyVal.ImpliedType()
		if err != nil {
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				errSummary,
				fmt.Sprintf("Invalid value for variable %q recorded in plan file: %s.", err),
			))
			continue
		}
		val, err := dyVal.Decode(ty)
		if err != nil {
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				errSummary,
				fmt.Sprintf("Invalid value for variable %q recorded in plan file: %s.", err),
			))
			continue
		}

		variables[name] = &terraform.InputValue{
			Value:      val,
			SourceType: terraform.ValueFromPlan,
		}
	}
	opts.Variables = variables

	// TODO: populate the changes (formerly diff)
	// TODO: targets
	// TODO: check that the states match
	// TODO: impose provider SHA256 constraints

	tfCtx, ctxDiags := terraform.NewContext(&opts)
	diags = diags.Append(ctxDiags)
	return tfCtx, sources, diags
}

const validateWarnHeader = `
There are warnings related to your configuration. If no errors occurred,
Terraform will continue despite these warnings. It is a good idea to resolve
these warnings in the near future.

Warnings:
`
