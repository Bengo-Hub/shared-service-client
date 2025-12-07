# Deployment Guide for Shared Service Client

## Overview

This guide covers how to deploy and use `shared-service-client` in production and development environments.

## Repository Structure

**Important:** `shared-service-client` is an **independent GitHub repository** that must be created in the `Bengo-Hub` organization.

**Repository URL:** `https://github.com/Bengo-Hub/shared-service-client`

## Production Deployment

### Git-Based Import (Recommended for Production)

Since all services are in the same GitHub organization (`Bengo-Hub`), use git-based imports with tagged versions:

#### Setup Steps:

1. **Tag the library repository:**
   ```bash
   cd shared-service-client/
   git tag v0.1.0
   git push origin v0.1.0
   ```

2. **Service go.mod** (no replace directive needed):
   ```go
   require (
       github.com/Bengo-Hub/shared-service-client v0.1.0
   )
   ```

3. **Configure Git credentials** (for private repos in CI/CD):
   ```bash
   git config --global url."https://${GITHUB_TOKEN}@github.com/".insteadOf "https://github.com/"
   ```

4. **In CI/CD pipelines**, set:
   ```bash
   export GOPRIVATE=github.com/Bengo-Hub/*
   export GONOPROXY=github.com/Bengo-Hub/*
   export GONOSUMDB=github.com/Bengo-Hub/*
   ```

5. **For local development**, if you don't use `go.work`, you can still use:
   ```bash
   go get github.com/Bengo-Hub/shared-service-client@v0.1.0
   ```

## CI/CD Configuration

### GitHub Actions Example

```yaml
- name: Setup Go
  uses: actions/setup-go@v4
  with:
    go-version: '1.24'
  
- name: Configure private modules
  run: |
    git config --global url."https://${{ secrets.GITHUB_TOKEN }}@github.com/".insteadOf "https://github.com/"
    export GOPRIVATE=github.com/Bengo-Hub/*
    export GONOPROXY=github.com/Bengo-Hub/*
    export GONOSUMDB=github.com/Bengo-Hub/*

- name: Build
  run: go build ./cmd/api
```

### Dockerfile Example

```dockerfile
FROM golang:1.24-alpine AS builder

# Configure private modules
ARG GITHUB_TOKEN
RUN git config --global url."https://${GITHUB_TOKEN}@github.com/".insteadOf "https://github.com/"
ENV GOPRIVATE=github.com/Bengo-Hub/*
ENV GONOPROXY=github.com/Bengo-Hub/*
ENV GONOSUMDB=github.com/Bengo-Hub/*

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -o /app/api ./cmd/api
```

## Local Development with Go Workspace

For local development, use Go workspaces:

1. Clone all repositories into a parent directory
2. Create `go.work` at the BengoBox root
3. All modules see each other automatically

See `SETUP.md` for detailed instructions.

## Troubleshooting

### Issue: `cannot find module providing package`
**Solution**: Ensure the repository is created on GitHub and the tag exists.

### Issue: `401 Unauthorized` when fetching module
**Solution**: Configure `GOPRIVATE` and git credentials with GitHub token.

### Issue: `no matching versions`
**Solution**: Verify the tag exists: `git ls-remote --tags origin`

## Best Practices

1. **Always use tagged versions** in production (never `@latest` or `@main`)
2. **Pin versions** in `go.mod` for reproducible builds
3. **Test locally** with `go.work` before deploying
4. **Document breaking changes** in CHANGELOG.md
5. **Keep backward compatibility** within MAJOR versions

