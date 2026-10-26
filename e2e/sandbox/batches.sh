# shellcheck shell=bash
# shellcheck disable=SC2034
declare -A E2E_BATCHES=(
    [engines]="dumpload mysql postgres mongo redis elasticsearch s3"
    [fw1]="fw_alembic fw_diesel fw_django fw_flyway fw_laravel fw_rails fw_sqlx"
    [fw2]="fw_drizzle fw_golang_migrate fw_knex fw_mikro fw_prisma fw_sequelize fw_typeorm"
    [matrix]="matrix_es matrix_mongo matrix_postgres matrix_redis containerref containerref-all conn_forms engine_aliases named_conns"
    [watcher]="autofetch branchscoped deltawatch headwatcher hook_cwd lifecycle main_worktree main_worktree_db oncheckout onfilechange onfilechange_redis watcher"
    [features]="clones_auto coldbuild_siblings compression ctrrestart extras fanout log_level logs misc patches poolmax race recover retention sighup snapshot_gc switchback teardown worktrees_root"
    [cli]="cli cli_engine cli_surface db_query mcp mcp_write mongo_dump"
    [lite]="cli_surface logs notifications file_engine"
)
E2E_ORDER=(engines fw1 fw2 matrix watcher features cli)
