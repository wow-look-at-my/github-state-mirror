UPDATE pull_requests
		SET merge_stale_sha = ?, merge_stale_at = ?
		WHERE owner = 'org1' AND repo = 'repo1' AND number = 7