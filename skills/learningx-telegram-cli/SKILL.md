---
name: learningx-telegram-cli
description: Run and troubleshoot the lx-agent Telegram + LearningX stack through its CLI commands. Use when you need to inspect config, list courses/assignments/files/announcements, run bot/serve modes, or execute operational checks from the command line.
---

# LearningX Telegram CLI

Use this skill to operate `lx-agent` via CLI.

## Command Bridge

Run commands through the bundled bridge script:

```bash
skills/learningx-telegram-cli/scripts/run-lx-agent-cli.sh <command> [args...]
```

Set `LX_AGENT_ROOT` when running outside the repository root.

## Common Commands

```bash
skills/learningx-telegram-cli/scripts/run-lx-agent-cli.sh config
skills/learningx-telegram-cli/scripts/run-lx-agent-cli.sh courses
skills/learningx-telegram-cli/scripts/run-lx-agent-cli.sh assignments
skills/learningx-telegram-cli/scripts/run-lx-agent-cli.sh files
skills/learningx-telegram-cli/scripts/run-lx-agent-cli.sh announcements
skills/learningx-telegram-cli/scripts/run-lx-agent-cli.sh bot
skills/learningx-telegram-cli/scripts/run-lx-agent-cli.sh serve
```

## Notes

- The bridge runs `go run ./cmd/lx-agent ...`.
- Keep outputs concise and include command results directly in your response.
- For bot/serve runs, surface startup errors and required env/config clearly.
