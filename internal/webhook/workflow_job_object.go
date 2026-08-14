package webhook

import "encoding/json"

// WorkflowJobObject is the raw `workflow_job` key of a workflow_job delivery.
//
// ParseWorkflowJobPayload reads the same delivery for the TRUTH side, flattened
// into the columns that table keeps. This returns the object UNTOUCHED instead,
// because its consumer is the response cache, whose stored job carries fields
// the truth table has no column for -- labels and the whole steps array. What
// that object BECOMES is decided in exactly one place (ghdata.TrimWorkflowJob),
// the same place a fetched job goes through, so a rewritten answer and a
// fetched one cannot drift apart.
func WorkflowJobObject(raw json.RawMessage) (json.RawMessage, bool) {
	var body struct {
		WorkflowJob json.RawMessage `json:"workflow_job"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || len(body.WorkflowJob) == 0 {
		return nil, false
	}
	return body.WorkflowJob, true
}
