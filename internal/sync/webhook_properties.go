// Custom property deliveries, applied to the stored answers of GET /orgs/{org}/properties/values.

package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

// Custom property deliveries, applied to the stored answers of
// GET /orgs/{org}/properties/values.

type customPropertyPayload struct {
	Organization struct {
		Login string `json:"login"`
	} `json:"organization"`
	Repository struct {
		ID    int64 `json:"id"`
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
	} `json:"repository"`
	NewPropertyValues []ghdata.OrgPropertyValue `json:"new_property_values"`
}

func (p customPropertyPayload) org() string {
	if p.Organization.Login != "" {
		return p.Organization.Login
	}
	return p.Repository.Owner.Login
}

// onCustomPropertyValues writes a repo's new property values into every
// stored page that lists the repo. A change a page cannot take in place
// drops the org's pages. see docs/webhooks/invalidations.md
func (d *WebhookDispatcher) onCustomPropertyValues(ctx context.Context, event webhook.Event) outcome {
	var p customPropertyPayload
	if err := json.Unmarshal(event.Raw, &p); err != nil || p.org() == "" {
		slog.Warn("webhook: unparseable custom_property_values payload", "error", err)
		return ignored("unparseable custom_property_values payload")
	}
	org := p.org()
	if p.Repository.ID > 0 {
		ok, err := d.store.ApplyOrgPropertyValues(ctx, org, p.Repository.ID, p.NewPropertyValues, time.Now())
		if err != nil {
			slog.Warn("webhook: apply property values failed", "org", org, "error", err)
		}
		if err == nil && ok {
			return applied(fmt.Sprintf("property values of repo %d written into %s's stored pages", p.Repository.ID, org))
		}
	}
	if err := d.store.InvalidateOrgPropertyValues(ctx, org); err != nil {
		slog.Warn("webhook: flush property values failed", "org", org, "error", err)
		return errored("flush property values failed")
	}
	return outcome{disposition: webhook.DispInvalidated, detail: "property values change the pages cannot take in place; dropped " + org + "'s pages"}
}

// onCustomProperty drops the org's pages. A created or deleted definition
// adds or removes a property on every repo, and the payload states only the
// definition. see docs/webhooks/invalidations.md
func (d *WebhookDispatcher) onCustomProperty(ctx context.Context, event webhook.Event) outcome {
	var p customPropertyPayload
	if err := json.Unmarshal(event.Raw, &p); err != nil || p.org() == "" {
		slog.Warn("webhook: unparseable custom_property payload", "error", err)
		return ignored("unparseable custom_property payload")
	}
	org := p.org()
	if err := d.store.InvalidateOrgPropertyValues(ctx, org); err != nil {
		slog.Warn("webhook: flush property values failed", "org", org, "error", err)
		return errored("flush property values failed")
	}
	return outcome{disposition: webhook.DispInvalidated, detail: "property definition " + event.Action + "; dropped " + org + "'s pages"}
}
