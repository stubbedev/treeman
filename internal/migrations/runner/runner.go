// Package runner invokes a framework's migrate command against a
// target database. Frameworks read connection settings from their
// own config (Laravel .env, Rails config/database.yml, etc.); we
// override the target DB name with framework-appropriate env vars.
package runner

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

// Mode — Up runs all migrations from scratch; Pending applies only
// the newly-added migrations.
type Mode int

const (
	ModeUp Mode = iota
	ModePending
)

// Outcome is the result of one migrate invocation.
type Outcome struct {
	ExitCode   int
	StdoutTail string
	StderrTail string
}

// Run executes the framework's migrate command. `inheritedEnv` is
// the env captured at CLI invocation (clears the daemon's env, layers
// the caller's PATH-aware env on top, then the per-framework knobs).
func Run(
	ctx context.Context,
	framework string,
	repoRoot string,
	targetDB string,
	mode Mode,
	connEnvOverrides map[string]string,
	inheritedEnv map[string]string,
) (Outcome, error) {
	bin, args, err := commandFor(framework, mode)
	if err != nil {
		return Outcome{ExitCode: -1}, err
	}
	c := exec.CommandContext(ctx, bin, args...)
	c.Dir = repoRoot

	env := make([]string, 0, len(inheritedEnv)+len(connEnvOverrides)+8)
	for k, v := range inheritedEnv {
		env = append(env, k+"="+v)
	}
	env = append(env, "TREEMAN_TARGET_DB="+targetDB)
	for k, v := range frameworkEnv(framework, targetDB) {
		env = append(env, k+"="+v)
	}
	for k, v := range connEnvOverrides {
		env = append(env, k+"="+v)
	}
	c.Env = env

	var stdout, stderr bytes.Buffer
	c.Stdout = tailWriter(&stdout, 16*1024)
	c.Stderr = tailWriter(&stderr, 16*1024)

	err = c.Run()
	out := Outcome{
		StdoutTail: stdout.String(),
		StderrTail: stderr.String(),
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		out.ExitCode = exitErr.ExitCode()
		return out, nil
	}
	if err != nil {
		out.ExitCode = -1
		return out, err
	}
	out.ExitCode = 0
	return out, nil
}

func commandFor(framework string, mode Mode) (string, []string, error) {
	_ = mode // all frameworks use the same migrate-up command today
	switch framework {
	case "laravel":
		return "php", []string{"artisan", "migrate", "--force"}, nil
	case "rails":
		return "bin/rails", []string{"db:migrate"}, nil
	case "django":
		return "python", []string{"manage.py", "migrate", "--noinput"}, nil
	case "golang-migrate":
		return "migrate", []string{"up"}, nil
	case "sqlx-cli":
		return "sqlx", []string{"migrate", "run"}, nil
	case "diesel":
		return "diesel", []string{"migration", "run"}, nil
	case "prisma":
		return "npx", []string{"prisma", "migrate", "deploy"}, nil
	case "knex":
		return "npx", []string{"knex", "migrate:latest"}, nil
	case "alembic":
		return "alembic", []string{"upgrade", "head"}, nil
	case "flyway":
		return "flyway", []string{"migrate"}, nil
	case "typeorm":
		return "npx", []string{"typeorm", "migration:run"}, nil
	case "drizzle":
		return "npx", []string{"drizzle-kit", "migrate"}, nil
	case "sequelize":
		return "npx", []string{"sequelize", "db:migrate"}, nil
	case "mikro-orm":
		return "npx", []string{"mikro-orm", "migration:up"}, nil
	}
	return "", nil, fmt.Errorf("unknown migration framework: %s", framework)
}

func frameworkEnv(framework, targetDB string) map[string]string {
	switch framework {
	case "laravel":
		return map[string]string{
			"DB_DATABASE":      targetDB,
			"DB_TEST_DATABASE": targetDB,
		}
	case "rails":
		return map[string]string{"DATABASE": targetDB}
	case "django":
		return map[string]string{"DJANGO_DB_NAME": targetDB}
	case "golang-migrate":
		return map[string]string{"MIGRATE_DATABASE_NAME": targetDB}
	case "sqlx-cli":
		return map[string]string{"DATABASE_URL_NAME": targetDB}
	case "alembic", "flyway", "prisma", "knex", "typeorm", "drizzle", "sequelize", "mikro-orm", "diesel":
		return map[string]string{"DATABASE_NAME": targetDB}
	}
	return nil
}

// tailWriter caps a bytes.Buffer at `n` bytes by truncating the
// front when growing. Cheap and lossy on the head; good enough for
// driver logs.
func tailWriter(buf *bytes.Buffer, n int) *capWriter {
	return &capWriter{buf: buf, cap: n}
}

type capWriter struct {
	buf *bytes.Buffer
	cap int
}

func (w *capWriter) Write(p []byte) (int, error) {
	if _, err := w.buf.Write(p); err != nil {
		return 0, err
	}
	if w.buf.Len() > w.cap {
		drop := w.buf.Len() - w.cap
		w.buf.Next(drop)
	}
	return len(p), nil
}
