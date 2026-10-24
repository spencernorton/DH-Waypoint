package processors

import v1 "github.com/spencernorton/DH-Waypoint/processorapi/v1"

const retiredLLMReason = "builtin_llm_retired_use_http_processor"
const retiredLearningReason = "builtin_learning_retired_use_http_processor"

// Retained registrations and queued plans must fail closed after an upgrade.
// Their history remains readable; replacing the registration fences old work.
func retiredBuiltinReason(driver string, learning *v1.LearningConfig) string {
	if driver == v1.DriverBuiltinLLM {
		return retiredLLMReason
	}
	if driver != "" && driver != v1.DriverHTTP && learning != nil && learning.Enabled &&
		learning.Mode != "" && learning.Mode != "disabled" && learning.Mode != "log_only" {
		return retiredLearningReason
	}
	return ""
}
