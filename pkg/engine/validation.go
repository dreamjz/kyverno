package engine

import (
	"encoding/json"
	"fmt"
	"github.com/kyverno/kyverno/pkg/engine/common"
	"github.com/pkg/errors"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	"reflect"
	"strings"
	"time"

	"github.com/go-logr/logr"
	gojmespath "github.com/jmespath/go-jmespath"
	kyverno "github.com/kyverno/kyverno/pkg/api/kyverno/v1"
	"github.com/kyverno/kyverno/pkg/engine/response"
	"github.com/kyverno/kyverno/pkg/engine/utils"
	"github.com/kyverno/kyverno/pkg/engine/validate"
	"github.com/kyverno/kyverno/pkg/engine/variables"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

//Validate applies validation rules from policy on the resource
func Validate(policyContext *PolicyContext) (resp *response.EngineResponse) {
	resp = &response.EngineResponse{}
	startTime := time.Now()

	logger := buildLogger(policyContext)
	logger.V(4).Info("start policy processing", "startTime", startTime)
	defer func() {
		buildResponse(policyContext, resp, startTime)
		logger.V(4).Info("finished policy processing", "processingTime", resp.PolicyResponse.ProcessingTime.String(), "validationRulesApplied", resp.PolicyResponse.RulesAppliedCount)
	}()

	resp = validateResource(logger, policyContext)
	return
}

func buildLogger(ctx *PolicyContext) logr.Logger {
	logger := log.Log.WithName("EngineValidate").WithValues("policy", ctx.Policy.Name)
	if reflect.DeepEqual(ctx.NewResource, unstructured.Unstructured{}) {
		logger = logger.WithValues("kind", ctx.OldResource.GetKind(), "namespace", ctx.OldResource.GetNamespace(), "name", ctx.OldResource.GetName())
	} else {
		logger = logger.WithValues("kind", ctx.NewResource.GetKind(), "namespace", ctx.NewResource.GetNamespace(), "name", ctx.NewResource.GetName())
	}

	return logger
}

func buildResponse(ctx *PolicyContext, resp *response.EngineResponse, startTime time.Time) {
	if reflect.DeepEqual(resp, response.EngineResponse{}) {
		return
	}

	if reflect.DeepEqual(resp.PatchedResource, unstructured.Unstructured{}) {
		// for delete requests patched resource will be oldResource since newResource is empty
		var resource = ctx.NewResource
		if reflect.DeepEqual(ctx.NewResource, unstructured.Unstructured{}) {
			resource = ctx.OldResource
		}

		resp.PatchedResource = resource
	}

	resp.PolicyResponse.Policy.Name = ctx.Policy.GetName()
	resp.PolicyResponse.Policy.Namespace = ctx.Policy.GetNamespace()
	resp.PolicyResponse.Resource.Name = resp.PatchedResource.GetName()
	resp.PolicyResponse.Resource.Namespace = resp.PatchedResource.GetNamespace()
	resp.PolicyResponse.Resource.Kind = resp.PatchedResource.GetKind()
	resp.PolicyResponse.Resource.APIVersion = resp.PatchedResource.GetAPIVersion()
	resp.PolicyResponse.ValidationFailureAction = ctx.Policy.Spec.ValidationFailureAction
	resp.PolicyResponse.ProcessingTime = time.Since(startTime)
	resp.PolicyResponse.PolicyExecutionTimestamp = startTime.Unix()
}

func incrementAppliedCount(resp *response.EngineResponse) {
	resp.PolicyResponse.RulesAppliedCount++
}

func incrementErrorCount(resp *response.EngineResponse) {
	resp.PolicyResponse.RulesErrorCount++
}

func validateResource(log logr.Logger, ctx *PolicyContext) *response.EngineResponse {
	resp := &response.EngineResponse{}
	if ManagedPodResource(ctx.Policy, ctx.NewResource) {
		log.V(5).Info("skip validation of pods managed by workload controllers", "policy", ctx.Policy.GetName())
		return resp
	}

	ctx.JSONContext.Checkpoint()
	defer ctx.JSONContext.Restore()

	for _, rule := range ctx.Policy.Spec.Rules {
		if !rule.HasValidate() {
			continue
		}

		log = log.WithValues("rule", rule.Name)
		if !matches(log, rule, ctx) {
			continue
		}

		log.V(3).Info("matched validate rule")
		ctx.JSONContext.Reset()
		startTime := time.Now()

		ruleResp := processValidationRule(log, ctx, &rule)
		if ruleResp != nil {
			addRuleResponse(log, resp, ruleResp, startTime)
		}
	}

	return resp
}

func processValidationRule(log logr.Logger, ctx *PolicyContext, rule *kyverno.Rule) *response.RuleResponse {
	v := newValidator(log, ctx, rule)
	if rule.Validation.ForEachValidation != nil {
		return v.validateForEach()
	}

	return v.validate()
}

func addRuleResponse(log logr.Logger, resp *response.EngineResponse, ruleResp *response.RuleResponse, startTime time.Time) {
	ruleResp.RuleStats.ProcessingTime = time.Since(startTime)
	ruleResp.RuleStats.RuleExecutionTimestamp = startTime.Unix()
	log.V(4).Info("finished processing rule", "processingTime", ruleResp.RuleStats.ProcessingTime.String())

	if ruleResp.Status == response.RuleStatusPass || ruleResp.Status == response.RuleStatusFail {
		incrementAppliedCount(resp)
	} else if ruleResp.Status == response.RuleStatusError {
		incrementErrorCount(resp)
	}

	resp.PolicyResponse.Rules = append(resp.PolicyResponse.Rules, *ruleResp)
}

type validator struct {
	log              logr.Logger
	ctx              *PolicyContext
	rule             *kyverno.Rule
	contextEntries   []kyverno.ContextEntry
	anyAllConditions apiextensions.JSON
	pattern          apiextensions.JSON
	anyPattern       apiextensions.JSON
	deny             *kyverno.Deny
}

func newValidator(log logr.Logger, ctx *PolicyContext, rule *kyverno.Rule) *validator {
	ruleCopy := rule.DeepCopy()
	return &validator{
		log:              log,
		rule:             ruleCopy,
		ctx:              ctx,
		contextEntries:   ruleCopy.Context,
		anyAllConditions: ruleCopy.AnyAllConditions,
		pattern:          ruleCopy.Validation.Pattern,
		anyPattern:       ruleCopy.Validation.AnyPattern,
		deny:             ruleCopy.Validation.Deny,
	}
}

func newForeachValidator(log logr.Logger, ctx *PolicyContext, rule *kyverno.Rule) *validator {
	ruleCopy := rule.DeepCopy()

	// Variable substitution expects JSON data, so we convert to a map
	anyAllConditions, err := common.ToMap(ruleCopy.Validation.ForEachValidation.AnyAllConditions)
	if err != nil {
		log.Error(err, "failed to convert ruleCopy.Validation.ForEachValidation.AnyAllConditions")
	}

	return &validator{
		log:              log,
		ctx:              ctx,
		rule:             ruleCopy,
		contextEntries:   ruleCopy.Validation.ForEachValidation.Context,
		anyAllConditions: anyAllConditions,
		pattern:          ruleCopy.Validation.ForEachValidation.Pattern,
		anyPattern:       ruleCopy.Validation.ForEachValidation.AnyPattern,
		deny:             ruleCopy.Validation.ForEachValidation.Deny,
	}
}

func (v *validator) validate() *response.RuleResponse {
	if err := v.loadContext(); err != nil {
		return ruleError(v.rule, "failed to load context", err)
	}

	preconditionsPassed, err := v.checkPreconditions()
	if err != nil {
		return ruleError(v.rule, "failed to evaluate preconditions", err)
	} else if !preconditionsPassed {
		return ruleResponse(v.rule, "preconditions not met", response.RuleStatusSkip)
	}

	if v.pattern != nil || v.anyPattern != nil {
		if err = v.substitutePatterns(); err != nil {
			return ruleError(v.rule, "variable substitution failed", err)
		}

		ruleResponse := v.validateResourceWithRule()
		return ruleResponse

	} else if v.deny != nil {
		ruleResponse := v.validateDeny()
		return ruleResponse
	}

	v.log.Info("invalid validation rule: either patterns or deny conditions are expected")
	return nil
}

func (v *validator) validateForEach() *response.RuleResponse {
	if err := v.loadContext(); err != nil {
		return ruleError(v.rule, "failed to load context", err)
	}

	preconditionsPassed, err := v.checkPreconditions()
	if err != nil {
		return ruleError(v.rule, "failed to evaluate preconditions", err)
	} else if !preconditionsPassed {
		return ruleResponse(v.rule, "preconditions not met", response.RuleStatusSkip)
	}

	foreach := v.rule.Validation.ForEachValidation
	if foreach == nil {
		return nil
	}

	elements, err := v.evaluateList(foreach.List)
	if err != nil {
		msg := fmt.Sprintf("failed to evaluate list %s", foreach.List)
		return ruleError(v.rule, msg, err)
	}

	v.ctx.JSONContext.Checkpoint()
	defer v.ctx.JSONContext.Restore()

	applyCount := 0
	for _, e := range elements {
		v.ctx.JSONContext.Reset()

		ctx := v.ctx.Copy()
		if err := addElementToContext(ctx, e); err != nil {
			v.log.Error(err, "failed to add element to context")
			return ruleError(v.rule, "failed to process foreach", err)
		}

		foreachValidator := newForeachValidator(v.log, ctx, v.rule)
		r := foreachValidator.validate()
		if r == nil {
			v.log.Info("skipping rule due to empty result")
			continue
		} else if r.Status == response.RuleStatusSkip {
			v.log.Info("skipping rule as preconditions were not met")
			continue
		} else if r.Status != response.RuleStatusPass {
			msg := fmt.Sprintf("validation failed in foreach rule for %v", r.Message)
			return ruleResponse(v.rule, msg, r.Status)
		}

		applyCount++
	}

	if applyCount == 0 {
		return ruleResponse(v.rule, "rule skipped", response.RuleStatusSkip)
	}

	return ruleResponse(v.rule, "rule passed", response.RuleStatusPass)
}

func addElementToContext(ctx *PolicyContext, e interface{}) error {
	data, err := common.ToMap(e)
	if err != nil {
		return err
	}

	u := unstructured.Unstructured{}
	u.SetUnstructuredContent(data)
	ctx.NewResource = u

	if err := ctx.JSONContext.AddResourceAsObject(e); err != nil {
		return errors.Wrapf(err, "failed to add resource (%v) to JSON context", e)
	}

	return nil
}

func (v *validator) evaluateList(jmesPath string) ([]interface{}, error) {
	i, err := v.ctx.JSONContext.Query(jmesPath)
	if err != nil {
		return nil, err
	}

	l, ok := i.([]interface{})
	if !ok {
		return []interface{}{i}, nil
	}

	return l, nil
}

func (v *validator) loadContext() error {
	if err := LoadContext(v.log, v.contextEntries, v.ctx.ResourceCache, v.ctx, v.rule.Name); err != nil {
		if _, ok := err.(gojmespath.NotFoundError); ok {
			v.log.V(3).Info("failed to load context", "reason", err.Error())
		} else {
			v.log.Error(err, "failed to load context")
		}

		return err
	}

	return nil
}

func (v *validator) checkPreconditions() (bool, error) {
	preconditions, err := variables.SubstituteAllInPreconditions(v.log, v.ctx.JSONContext, v.anyAllConditions)
	if err != nil {
		return false, errors.Wrapf(err, "failed to substitute variables in preconditions")
	}

	typeConditions, err := transformConditions(preconditions)
	if err != nil {
		return false, errors.Wrapf(err, "failed to parse preconditions")
	}

	pass := variables.EvaluateConditions(v.log, v.ctx.JSONContext, typeConditions)
	return pass, nil
}

func (v *validator) validateDeny() *response.RuleResponse {
	anyAllCond := v.deny.AnyAllConditions
	anyAllCond, err := variables.SubstituteAll(v.log, v.ctx.JSONContext, anyAllCond)
	if err != nil {
		return ruleError(v.rule, "failed to substitute variables in deny conditions", err)
	}

	if err = v.substituteDeny(); err != nil {
		return ruleError(v.rule, "failed to substitute variables in rule", err)
	}

	denyConditions, err := transformConditions(anyAllCond)
	if err != nil {
		return ruleError(v.rule, "invalid deny conditions", err)
	}

	deny := variables.EvaluateConditions(v.log, v.ctx.JSONContext, denyConditions)
	if deny {
		return ruleResponse(v.rule, v.getDenyMessage(deny), response.RuleStatusFail)
	}

	return ruleResponse(v.rule, v.getDenyMessage(deny), response.RuleStatusPass)
}

func (v *validator) getDenyMessage(deny bool) string {
	if !deny {
		return fmt.Sprintf("validation rule '%s' passed.", v.rule.Name)
	}

	msg := v.rule.Validation.Message
	if msg == "" {
		return fmt.Sprintf("validation error: rule %s failed", v.rule.Name)
	}

	raw, err := variables.SubstituteAll(v.log, v.ctx.JSONContext, msg)
	if err != nil {
		return msg
	}

	return raw.(string)
}

func (v *validator) validateResourceWithRule() *response.RuleResponse {
	if reflect.DeepEqual(v.ctx.OldResource, unstructured.Unstructured{}) {
		resp := v.validatePatterns(v.ctx.NewResource)
		return resp
	}

	if reflect.DeepEqual(v.ctx.NewResource, unstructured.Unstructured{}) {
		v.log.V(3).Info("skipping validation on deleted resource")
		return nil
	}

	oldResp := v.validatePatterns(v.ctx.OldResource)
	newResp := v.validatePatterns(v.ctx.NewResource)
	if isSameRuleResponse(oldResp, newResp) {
		v.log.V(3).Info("skipping modified resource as validation results have not changed")
		return nil
	}

	return newResp
}

// matches checks if either the new or old resource satisfies the filter conditions defined in the rule
func matches(logger logr.Logger, rule kyverno.Rule, ctx *PolicyContext) bool {
	err := MatchesResourceDescription(ctx.NewResource, rule, ctx.AdmissionInfo, ctx.ExcludeGroupRole, ctx.NamespaceLabels)
	if err == nil {
		return true
	}

	if !reflect.DeepEqual(ctx.OldResource, unstructured.Unstructured{}) {
		err := MatchesResourceDescription(ctx.OldResource, rule, ctx.AdmissionInfo, ctx.ExcludeGroupRole, ctx.NamespaceLabels)
		if err == nil {
			return true
		}
	}

	logger.V(4).Info("resource does not match rule", "reason", err.Error())
	return false
}

func isSameRuleResponse(r1 *response.RuleResponse, r2 *response.RuleResponse) bool {
	if r1.Name != r2.Name {
		return false
	}

	if r1.Type != r2.Type {
		return false
	}

	if r1.Message != r2.Message {
		return false
	}

	if r1.Status != r2.Status {
		return false
	}

	return true
}

// validatePatterns validate pattern and anyPattern
func (v *validator) validatePatterns(resource unstructured.Unstructured) *response.RuleResponse {
	if v.pattern != nil {
		if err := validate.MatchPattern(v.log, resource.Object, v.pattern); err != nil {

			if pe, ok := err.(*validate.PatternError); ok {
				v.log.V(3).Info("validation error", "path", pe.Path, "error", err.Error())
				if pe.Path == "" {
					return ruleResponse(v.rule, v.buildErrorMessage(err, ""), response.RuleStatusError)
				}

				return ruleResponse(v.rule, v.buildErrorMessage(err, pe.Path), response.RuleStatusFail)
			}
		}

		v.log.V(4).Info("successfully processed rule")
		msg := fmt.Sprintf("validation rule '%s' passed.", v.rule.Name)
		return ruleResponse(v.rule, msg, response.RuleStatusPass)
	}

	if v.anyPattern != nil {
		var failedAnyPatternsErrors []error
		var err error

		anyPatterns, err := deserializeAnyPattern(v.anyPattern)
		if err != nil {
			msg := fmt.Sprintf("failed to deserialize anyPattern, expected type array: %v", err)
			return ruleResponse(v.rule, msg, response.RuleStatusError)
		}

		for idx, pattern := range anyPatterns {
			err := validate.MatchPattern(v.log, resource.Object, pattern)
			if err == nil {
				msg := fmt.Sprintf("validation rule '%s' anyPattern[%d] passed.", v.rule.Name, idx)
				return ruleResponse(v.rule, msg, response.RuleStatusPass)
			}

			if pe, ok := err.(*validate.PatternError); ok {
				v.log.V(3).Info("validation rule failed", "anyPattern[%d]", idx, "path", pe.Path)
				if pe.Path == "" {
					patternErr := fmt.Errorf("Rule %s[%d] failed: %s.", v.rule.Name, idx, err.Error())
					failedAnyPatternsErrors = append(failedAnyPatternsErrors, patternErr)
				} else {
					patternErr := fmt.Errorf("Rule %s[%d] failed at path %s.", v.rule.Name, idx, pe.Path)
					failedAnyPatternsErrors = append(failedAnyPatternsErrors, patternErr)
				}
			}
		}

		// Any Pattern validation errors
		if len(failedAnyPatternsErrors) > 0 {
			var errorStr []string
			for _, err := range failedAnyPatternsErrors {
				errorStr = append(errorStr, err.Error())
			}

			v.log.V(4).Info(fmt.Sprintf("Validation rule '%s' failed. %s", v.rule.Name, errorStr))
			msg := buildAnyPatternErrorMessage(v.rule, errorStr)
			return ruleResponse(v.rule, msg, response.RuleStatusFail)
		}
	}

	return ruleResponse(v.rule, v.rule.Validation.Message, response.RuleStatusPass)
}

func deserializeAnyPattern(anyPattern apiextensions.JSON) ([]interface{}, error) {
	if anyPattern == nil {
		return nil, nil
	}

	ap, err := json.Marshal(anyPattern)
	if err != nil {
		return nil, err
	}

	var res []interface{}
	if err := json.Unmarshal(ap, &res); err != nil {
		return nil, err
	}

	return res, nil
}

func (v *validator) buildErrorMessage(err error, path string) string {
	if v.rule.Validation.Message == "" {
		if path != "" {
			return fmt.Sprintf("validation error: rule %s failed at path %s", v.rule.Name, path)
		}

		return fmt.Sprintf("validation error: rule %s execution error: %s", v.rule.Name, err.Error())
	}

	msgRaw, err := variables.SubstituteAll(v.log, v.ctx.JSONContext, v.rule.Validation.Message)
	if err != nil {
		v.log.Info("failed to substitute variables in message: %v", err)
	}

	msg := msgRaw.(string)
	if !strings.HasSuffix(msg, ".") {
		msg = msg + "."
	}

	if path != "" {
		return fmt.Sprintf("validation error: %s Rule %s failed at path %s", msg, v.rule.Name, path)
	}

	return fmt.Sprintf("validation error: %s Rule %s execution error: %s", msg, v.rule.Name, err.Error())
}

func buildAnyPatternErrorMessage(rule *kyverno.Rule, errors []string) string {
	errStr := strings.Join(errors, " ")
	if rule.Validation.Message == "" {
		return fmt.Sprintf("validation error: %s", errStr)
	}

	if strings.HasSuffix(rule.Validation.Message, ".") {
		return fmt.Sprintf("validation error: %s %s", rule.Validation.Message, errStr)
	}

	return fmt.Sprintf("validation error: %s. %s", rule.Validation.Message, errStr)
}

func (v *validator) substitutePatterns() error {
	if v.pattern != nil {
		i, err := variables.SubstituteAll(v.log, v.ctx.JSONContext, v.pattern)
		if err != nil {
			return err
		}

		v.pattern = i.(apiextensions.JSON)
		return nil
	}

	if v.anyPattern != nil {
		i, err := variables.SubstituteAll(v.log, v.ctx.JSONContext, v.anyPattern)
		if err != nil {
			return err
		}

		v.anyPattern = i.(apiextensions.JSON)
		return nil
	}

	return nil
}

func (v *validator) substituteDeny() error {
	if v.deny == nil {
		return nil
	}

	i, err := variables.SubstituteAll(v.log, v.ctx.JSONContext, v.deny)
	if err != nil {
		return err
	}

	v.deny = i.(*kyverno.Deny)
	return nil
}

func ruleError(rule *kyverno.Rule, msg string, err error) *response.RuleResponse {
	msg = fmt.Sprintf("%s: %s", msg, err.Error())
	return ruleResponse(rule, msg, response.RuleStatusError)
}

func ruleResponse(rule *kyverno.Rule, msg string, status response.RuleStatus) *response.RuleResponse {
	return &response.RuleResponse{
		Name:    rule.Name,
		Type:    utils.Validation.String(),
		Message: msg,
		Status:  status,
	}
}
