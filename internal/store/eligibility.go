package store

const currentActiveUnfrozenPlanPredicate = `p.current_version_id = v.version_id
AND v.state = 'active'
AND NOT EXISTS (SELECT 1 FROM plan_terminal_states terminal WHERE terminal.version_id = v.version_id)
AND NOT EXISTS (SELECT 1 FROM completed_plan_versions completed WHERE completed.version_id = v.version_id)
AND NOT EXISTS (SELECT 1 FROM plan_freezes freeze WHERE freeze.version_id = v.version_id)`
