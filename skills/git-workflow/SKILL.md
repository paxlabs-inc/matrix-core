---
name: git-workflow
description: Git workflow and conventions for development. Use when creating branches, committing code, or creating PRs. Triggers on git, branch, commit, PR, pull request, merge, workflow.
allowed-tools: Bash, Read
---

# Git Usage Guidelines

## Branch Management
- **Main branch**: Typically `main`, `master`, or a custom branch (check project conventions)
- **Workflow**: main branch → create feature branch → develop → PR back to main
- **NEVER** work directly on the main branch → ALWAYS create feature branches

## Branch Naming
- Feature branches: `feat/description` or `feature/description`
- Bug fixes: `fix/description`
- Refactoring: `refactor/description`

## Commit Message Format
Use Conventional Commits format:
- `feat:` - New features
- `fix:` - Bug fixes
- `refactor:` - Code refactoring
- `perf:` / `optimize:` - Performance improvements
- `chore:` - Build, version, or non-code changes
- `docs:` - Documentation only

**Format**: `type: short description`
- Use lowercase
- Keep first line under 72 characters
- Include issue number if applicable: `fix: resolve login bug OK-12345`

**IMPORTANT - Claude Code commits**:
- Do NOT include "Generated with Claude Code" link
- Do NOT include "Co-Authored-By: Claude" signature
- Commit message should be indistinguishable from human-written commits

## PR Naming Convention
Follow the same format as commit messages:
- `feat: add dark mode support`
- `fix: resolve authentication timeout issue`
- `refactor: simplify payment processing logic`

## Common Git Commands

### Creating a Feature Branch
```bash
git checkout main
git pull origin main
git checkout -b feat/my-new-feature
```

### Committing Changes

**Option 1: Use /commit command (if available)**
```bash
/commit
```
The `/commit` command automatically runs pre-commit checks and creates a well-formatted commit message.

**Option 2: Manual commit with pre-checks**
```bash
# Stage your changes
git add .

# Run pre-commit checks (MANDATORY)
yarn lint:staged
yarn tsc:staged

# If checks pass, commit
git commit -m "feat: add user profile page"
```

**IMPORTANT**:
- NEVER commit code that fails linting or TypeScript compilation
- Pre-commit checks are mandatory

### Pushing and Creating PR
```bash
git push -u origin feat/my-new-feature
# Then create PR via GitHub UI or gh CLI
```

### Rebasing on Latest Main
```bash
git fetch origin
git rebase origin/main
```
