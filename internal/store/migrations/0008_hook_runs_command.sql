-- 0008: enrich hook_runs so `treeman logs hooks` can render the
-- group label and command without re-reading hook log files.
--
-- `command` stores the rendered shell string the driver ran (the same
-- value carried on hooks.GroupOutcome.Command). `group_idx` is the
-- ordinal of the group within the phase (0-based), so multiple groups
-- in the same phase remain distinguishable when rendered in a table.

ALTER TABLE hook_runs ADD COLUMN command   TEXT;
ALTER TABLE hook_runs ADD COLUMN group_idx INTEGER NOT NULL DEFAULT 0;
