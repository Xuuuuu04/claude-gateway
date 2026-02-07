# Project Structure

Updated: 2026-02-07

## Top-level Layout
```text
.
- .gitignore
- .history
- .local
- Dockerfile
- LICENSE
- README.md
- README_EN.md
- cmd
- docker-compose.yml
- docs
- gateway
- go.mod
- go.sum
- internal
- scripts
- src
```

## Conventions
- Keep executable/business code under src/ as the long-term target.
- Keep docs under docs/ (or doc/ for Cangjie projects).
- Keep local runtime artifacts and secrets out of version control.
