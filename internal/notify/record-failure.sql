
		UPDATE subscriptions SET
			consecutive_failures = ?, last_failure_at = ?, last_error = ?, updated_at = ?,
			active = ?,
			disabled_reason = CASE WHEN ? != '' THEN ? ELSE disabled_reason END
		WHERE id = ?