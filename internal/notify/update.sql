
		UPDATE subscriptions SET
			url = ?, secret = ?, repo_filters = ?, event_filters = ?, active = ?,
			consecutive_failures = ?, disabled_reason = ?, updated_at = ?
		WHERE id = ? AND principal = ?