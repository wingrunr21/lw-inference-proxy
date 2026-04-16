# Agent Instructions

## GitHub API Operations

All GitHub API operations — including looking up action SHAs, querying repository info, listing releases, inspecting tags, working with PRs and issues, and any other interaction with the GitHub API — must follow this preference order:

1. **`gh` CLI** (`gh api`, `gh repo`, `gh release`, etc.) — use this first. It is authenticated, handles pagination and rate limiting correctly, and is the preferred tool for all GitHub work in this project.
2. **WebFetch against `api.github.com`** — fall back to this if `gh` is unavailable or the specific operation fails.
3. **`curl`** — last resort only, when neither `gh` nor WebFetch is available.

Examples:
```bash
# Resolve an action tag to a commit SHA
gh api repos/actions/checkout/git/ref/tags/v4

# Dereference an annotated tag to its commit SHA
gh api repos/actions/checkout/git/tags/<sha>

# List releases
gh release list

# Query repo metadata
gh repo view owner/repo --json name,defaultBranchRef
```
