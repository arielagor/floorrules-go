# CLAUDE.md

Read and follow [AGENTS.md](AGENTS.md). It is the source of truth for conventions, the definition of done, and what you may not do. This file only adds Claude Code specifics.

- Author and edit files with the Write/Edit tools. Do not generate source through shell heredocs or `echo` redirects; on this Windows machine the Bash tool strips backslashes, which silently corrupts regexes and string literals.
- Go lives at `C:\Program Files\Go\bin`; linters and scanners at `%USERPROFILE%\go\bin`. Put both on PATH in the command rather than assuming them.
- The race detector needs cgo, and this machine has no gcc. Run race and integration tests in the `golang:1.27` container exactly as VERIFY.md shows. Do not drop `-race` from the claim when you couldn't run it; say it was not run.
- The test database is the `floorrules-pg` container on port 5544. Never connect to any other local Postgres.
- Before you summarise work, run the definition-of-done checks and paste the real tail of each output. If something failed and you fixed it, add it to BUILD-LOG.md under "What the checks caught", including what you got wrong.
- Commit at milestones with conventional prefixes. Never push.
