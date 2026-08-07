package store

const workflowInboxPlanPredicate = `(` + currentActivePlanPredicate + `) OR (
    ((p.state = 'projecting' AND v.state = 'projecting'
      AND NOT EXISTS (SELECT 1 FROM plan_terminal_states terminal WHERE terminal.version_id = v.version_id)
      AND NOT EXISTS (SELECT 1 FROM completed_plan_versions completed WHERE completed.version_id = v.version_id))
     OR EXISTS (SELECT 1 FROM completed_plan_versions completed WHERE completed.version_id = v.version_id))
    AND EXISTS (
        SELECT 1 FROM github_poll_cursors cursor
        WHERE cursor.repository = p.repository
          AND cursor.failure_kind = 'unrecoverable'
          AND cursor.recovery_state = 'consumed'
          AND cursor.recovery_plan_version_id = v.version_id
    )
) OR EXISTS (
    SELECT 1 FROM inbox_delivery_recovery_questions recovery
    JOIN workflow_questions recovery_question ON recovery_question.question_id = recovery.question_id
    WHERE recovery_question.version_id = v.version_id AND recovery_question.state = 'open'
)`
