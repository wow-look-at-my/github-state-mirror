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
	return namedObject(raw, func(b *deliveryObjects) json.RawMessage { return b.WorkflowJob })
}

// CheckRunObject is the raw `check_run` key of a check_run delivery, returned
// untouched for the same reason: what it BECOMES is decided once, in
// ghdata.TrimCheckRun, on the same path a fetched check run takes.
func CheckRunObject(raw json.RawMessage) (json.RawMessage, bool) {
	return namedObject(raw, func(b *deliveryObjects) json.RawMessage { return b.CheckRun })
}

// WorkflowRunObject is the raw `workflow_run` key of a workflow_run delivery.
func WorkflowRunObject(raw json.RawMessage) (json.RawMessage, bool) {
	return namedObject(raw, func(b *deliveryObjects) json.RawMessage { return b.WorkflowRun })
}

type deliveryObjects struct {
	WorkflowJob json.RawMessage `json:"workflow_job"`
	CheckRun    json.RawMessage `json:"check_run"`
	WorkflowRun json.RawMessage `json:"workflow_run"`
}

func namedObject(raw json.RawMessage, pick func(*deliveryObjects) json.RawMessage) (json.RawMessage, bool) {
	var body deliveryObjects
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, false
	}
	obj := pick(&body)
	if len(obj) == 0 {
		return nil, false
	}
	return obj, true
}
