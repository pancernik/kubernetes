/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package validation

import (
	"errors"
	"fmt"

	"github.com/google/go-cmp/cmp"
	v1 "k8s.io/api/core/v1"
	metav1validation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	componentbasevalidation "k8s.io/component-base/config/validation"
	v1helper "k8s.io/kubernetes/pkg/apis/core/v1/helper"
	"k8s.io/kubernetes/pkg/scheduler/apis/config"
)

// ValidateKubeSchedulerConfiguration ensures validation of the KubeSchedulerConfiguration struct
func ValidateKubeSchedulerConfiguration(cc *config.KubeSchedulerConfiguration) field.ErrorList {
	allErrs := field.ErrorList{}
	allErrs = append(allErrs, componentbasevalidation.ValidateClientConnectionConfiguration(&cc.ClientConnection, field.NewPath("clientConnection"))...)
	allErrs = append(allErrs, componentbasevalidation.ValidateLeaderElectionConfiguration(&cc.LeaderElection, field.NewPath("leaderElection"))...)

	profilesPath := field.NewPath("profiles")
	if len(cc.Profiles) == 0 {
		allErrs = append(allErrs, field.Required(profilesPath, ""))
	} else {
		existingProfiles := make(map[string]int, len(cc.Profiles))
		for i := range cc.Profiles {
			profile := &cc.Profiles[i]
			path := profilesPath.Index(i)
			allErrs = append(allErrs, validateKubeSchedulerProfile(path, profile)...)
			if idx, ok := existingProfiles[profile.SchedulerName]; ok {
				allErrs = append(allErrs, field.Duplicate(path.Child("schedulerName"), profilesPath.Index(idx).Child("schedulerName")))
			}
			existingProfiles[profile.SchedulerName] = i
		}
		allErrs = append(allErrs, validateCommonQueueSort(profilesPath, cc.Profiles)...)
	}
	for _, msg := range validation.IsValidSocketAddr(cc.HealthzBindAddress) {
		allErrs = append(allErrs, field.Invalid(field.NewPath("healthzBindAddress"), cc.HealthzBindAddress, msg))
	}
	for _, msg := range validation.IsValidSocketAddr(cc.MetricsBindAddress) {
		allErrs = append(allErrs, field.Invalid(field.NewPath("metricsBindAddress"), cc.MetricsBindAddress, msg))
	}
	if cc.PercentageOfNodesToScore < 0 || cc.PercentageOfNodesToScore > 100 {
		allErrs = append(allErrs, field.Invalid(field.NewPath("percentageOfNodesToScore"),
			cc.PercentageOfNodesToScore, "not in valid range [0-100]"))
	}
	if cc.PodInitialBackoffSeconds <= 0 {
		allErrs = append(allErrs, field.Invalid(field.NewPath("podInitialBackoffSeconds"),
			cc.PodInitialBackoffSeconds, "must be greater than 0"))
	}
	if cc.PodMaxBackoffSeconds < cc.PodInitialBackoffSeconds {
		allErrs = append(allErrs, field.Invalid(field.NewPath("podMaxBackoffSeconds"),
			cc.PodMaxBackoffSeconds, "must be greater than or equal to PodInitialBackoffSeconds"))
	}

	allErrs = append(allErrs, validateExtenders(field.NewPath("extenders"), cc.Extenders)...)
	return allErrs
}

func validateKubeSchedulerProfile(path *field.Path, profile *config.KubeSchedulerProfile) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(profile.SchedulerName) == 0 {
		allErrs = append(allErrs, field.Required(path.Child("schedulerName"), ""))
	}
	return allErrs
}

func validateCommonQueueSort(path *field.Path, profiles []config.KubeSchedulerProfile) field.ErrorList {
	allErrs := field.ErrorList{}
	var canon *config.PluginSet
	if profiles[0].Plugins != nil {
		canon = profiles[0].Plugins.QueueSort
	}
	for i := 1; i < len(profiles); i++ {
		var curr *config.PluginSet
		if profiles[i].Plugins != nil {
			curr = profiles[i].Plugins.QueueSort
		}
		if !cmp.Equal(canon, curr) {
			allErrs = append(allErrs, field.Invalid(path.Index(i).Child("plugins", "queueSort"), curr, "has to match for all profiles"))
		}
	}
	// TODO(#88093): Validate that all plugin configs for the queue sort extension match.
	return allErrs
}

// ValidatePolicy checks for errors in the Config
// It does not return early so that it can find as many errors as possible
func ValidatePolicy(policy config.Policy) error {
	var validationErrors []error

	priorities := make(map[string]config.PriorityPolicy, len(policy.Priorities))
	for _, priority := range policy.Priorities {
		if priority.Weight <= 0 || priority.Weight >= config.MaxWeight {
			validationErrors = append(validationErrors, fmt.Errorf("Priority %s should have a positive weight applied to it or it has overflown", priority.Name))
		}
		validationErrors = append(validationErrors, validateCustomPriorities(priorities, priority))
	}

	if extenderErrs := validateExtenders(field.NewPath("extenders"), policy.Extenders); len(extenderErrs) > 0 {
		validationErrors = append(validationErrors, extenderErrs.ToAggregate().Errors()...)
	}

	if policy.HardPodAffinitySymmetricWeight < 0 || policy.HardPodAffinitySymmetricWeight > 100 {
		validationErrors = append(validationErrors, field.Invalid(field.NewPath("hardPodAffinitySymmetricWeight"), policy.HardPodAffinitySymmetricWeight, "not in valid range [0-100]"))
	}
	return utilerrors.NewAggregate(validationErrors)
}

// validateExtenders validates the configured extenders for the Scheduler
func validateExtenders(fldPath *field.Path, extenders []config.Extender) field.ErrorList {
	allErrs := field.ErrorList{}
	binders := 0
	extenderManagedResources := sets.NewString()
	for i, extender := range extenders {
		path := fldPath.Index(i)
		if len(extender.PrioritizeVerb) > 0 && extender.Weight <= 0 {
			allErrs = append(allErrs, field.Invalid(path.Child("weight"),
				extender.Weight, "must have a positive weight applied to it"))
		}
		if extender.BindVerb != "" {
			binders++
		}
		for j, resource := range extender.ManagedResources {
			managedResourcesPath := path.Child("managedResources").Index(j)
			errs := validateExtendedResourceName(v1.ResourceName(resource.Name))
			for _, err := range errs {
				allErrs = append(allErrs, field.Invalid(managedResourcesPath.Child("name"),
					resource.Name, fmt.Sprintf("%+v", err)))
			}
			if extenderManagedResources.Has(resource.Name) {
				allErrs = append(allErrs, field.Invalid(managedResourcesPath.Child("name"),
					resource.Name, "duplicate extender managed resource name"))
			}
			extenderManagedResources.Insert(resource.Name)
		}
	}
	if binders > 1 {
		allErrs = append(allErrs, field.Invalid(fldPath, fmt.Sprintf("found %d extenders implementing bind", binders), "only one extender can implement bind"))
	}
	return allErrs
}

// validateCustomPriorities validates that:
// 1. RequestedToCapacityRatioRedeclared custom priority cannot be declared multiple times,
// 2. LabelPreference/ServiceAntiAffinity custom priorities can be declared multiple times,
// however the weights for each custom priority type should be the same.
func validateCustomPriorities(priorities map[string]config.PriorityPolicy, priority config.PriorityPolicy) error {
	verifyRedeclaration := func(priorityType string) error {
		if existing, alreadyDeclared := priorities[priorityType]; alreadyDeclared {
			return fmt.Errorf("Priority %q redeclares custom priority %q, from:%q", priority.Name, priorityType, existing.Name)
		}
		priorities[priorityType] = priority
		return nil
	}
	verifyDifferentWeights := func(priorityType string) error {
		if existing, alreadyDeclared := priorities[priorityType]; alreadyDeclared {
			if existing.Weight != priority.Weight {
				return fmt.Errorf("%s  priority %q has a different weight with %q", priorityType, priority.Name, existing.Name)
			}
		}
		priorities[priorityType] = priority
		return nil
	}
	if priority.Argument != nil {
		if priority.Argument.LabelPreference != nil {
			if err := verifyDifferentWeights("LabelPreference"); err != nil {
				return err
			}
		} else if priority.Argument.ServiceAntiAffinity != nil {
			if err := verifyDifferentWeights("ServiceAntiAffinity"); err != nil {
				return err
			}
		} else if priority.Argument.RequestedToCapacityRatioArguments != nil {
			if err := verifyRedeclaration("RequestedToCapacityRatio"); err != nil {
				return err
			}
		} else {
			return fmt.Errorf("No priority arguments set for priority %s", priority.Name)
		}
	}
	return nil
}

// validateExtendedResourceName checks whether the specified name is a valid
// extended resource name.
func validateExtendedResourceName(name v1.ResourceName) []error {
	var validationErrors []error
	for _, msg := range validation.IsQualifiedName(string(name)) {
		validationErrors = append(validationErrors, errors.New(msg))
	}
	if len(validationErrors) != 0 {
		return validationErrors
	}
	if !v1helper.IsExtendedResourceName(name) {
		validationErrors = append(validationErrors, fmt.Errorf("%s is an invalid extended resource name", name))
	}
	return validationErrors
}

// ValidateInterPodAffinityArgs validates that InterPodAffinityArgs are correct.
func ValidateInterPodAffinityArgs(args config.InterPodAffinityArgs) error {
	return ValidateHardPodAffinityWeight(field.NewPath("hardPodAffinityWeight"), args.HardPodAffinityWeight)
}

const (
	// MinHardPodAffinityWeight is the minimum HardPodAffinityWeight.
	MinHardPodAffinityWeight int32 = 0
	// MaxHardPodAffinityWeight is the maximum HardPodAffinityWeight.
	MaxHardPodAffinityWeight int32 = 100
)

// ValidateHardPodAffinityWeight validates that weight is within allowed range.
func ValidateHardPodAffinityWeight(path *field.Path, w int32) error {
	if w < MinHardPodAffinityWeight || w > MaxHardPodAffinityWeight {
		msg := fmt.Sprintf("not in valid range [%d-%d]", MinHardPodAffinityWeight, MaxHardPodAffinityWeight)
		return field.Invalid(path, w, msg)
	}
	return nil
}

// ValidateNodeLabelArgs validates that NodeLabelArgs are correct.
func ValidateNodeLabelArgs(args config.NodeLabelArgs) error {
	if err := validateNoConflict(args.PresentLabels, args.AbsentLabels); err != nil {
		return err
	}
	if err := validateNoConflict(args.PresentLabelsPreference, args.AbsentLabelsPreference); err != nil {
		return err
	}
	return nil
}

// validateNoConflict validates that presentLabels and absentLabels do not conflict.
func validateNoConflict(presentLabels []string, absentLabels []string) error {
	m := make(map[string]struct{}, len(presentLabels))
	for _, l := range presentLabels {
		m[l] = struct{}{}
	}
	for _, l := range absentLabels {
		if _, ok := m[l]; ok {
			return fmt.Errorf("detecting at least one label (e.g., %q) that exist in both the present(%+v) and absent(%+v) label list", l, presentLabels, absentLabels)
		}
	}
	return nil
}

// ValidatePodTopologySpreadArgs validates that PodTopologySpreadArgs are correct.
// It replicates the validation from pkg/apis/core/validation.validateTopologySpreadConstraints
// with an additional check for .labelSelector to be nil.
func ValidatePodTopologySpreadArgs(args *config.PodTopologySpreadArgs) error {
	var allErrs field.ErrorList
	path := field.NewPath("defaultConstraints")

	for i, c := range args.DefaultConstraints {
		p := path.Index(i)
		if c.MaxSkew <= 0 {
			f := p.Child("maxSkew")
			allErrs = append(allErrs, field.Invalid(f, c.MaxSkew, "must be greater than zero"))
		}
		allErrs = append(allErrs, validateTopologyKey(p.Child("topologyKey"), c.TopologyKey)...)
		if err := validateWhenUnsatisfiable(p.Child("whenUnsatisfiable"), c.WhenUnsatisfiable); err != nil {
			allErrs = append(allErrs, err)
		}
		if c.LabelSelector != nil {
			f := field.Forbidden(p.Child("labelSelector"), "constraint must not define a selector, as they deduced for each pod")
			allErrs = append(allErrs, f)
		}
		if err := validateConstraintNotRepeat(path, args.DefaultConstraints, i); err != nil {
			allErrs = append(allErrs, err)
		}
	}
	if len(allErrs) == 0 {
		return nil
	}
	return allErrs.ToAggregate()
}

func validateTopologyKey(p *field.Path, v string) field.ErrorList {
	var allErrs field.ErrorList
	if len(v) == 0 {
		allErrs = append(allErrs, field.Required(p, "can not be empty"))
	} else {
		allErrs = append(allErrs, metav1validation.ValidateLabelName(v, p)...)
	}
	return allErrs
}

var (
	supportedScheduleActions = sets.NewString(string(v1.DoNotSchedule), string(v1.ScheduleAnyway))
)

func validateWhenUnsatisfiable(p *field.Path, v v1.UnsatisfiableConstraintAction) *field.Error {
	if len(v) == 0 {
		return field.Required(p, "can not be empty")
	}
	if !supportedScheduleActions.Has(string(v)) {
		return field.NotSupported(p, v, supportedScheduleActions.List())
	}
	return nil
}

func validateConstraintNotRepeat(path *field.Path, constraints []v1.TopologySpreadConstraint, idx int) *field.Error {
	c := &constraints[idx]
	for i := range constraints[:idx] {
		other := &constraints[i]
		if c.TopologyKey == other.TopologyKey && c.WhenUnsatisfiable == other.WhenUnsatisfiable {
			return field.Duplicate(path.Index(idx), fmt.Sprintf("{%v, %v}", c.TopologyKey, c.WhenUnsatisfiable))
		}
	}
	return nil
}
