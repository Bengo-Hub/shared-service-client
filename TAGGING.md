# Tagging and Versioning Guide

## Repository Structure

**Important:** `shared-service-client` is an **independent GitHub repository** (`github.com/Bengo-Hub/shared-service-client`) in the Bengo-Hub organization. Each BengoBox service is also an independent repository. The `BengoBox` folder is just a local root directory where developers clone repositories - it is **not** a monorepo.

## Tagging the Library

### Step 1: Tag the Repository

Tag the `shared-service-client` repository:

```bash
# In the shared-service-client repository
cd shared-service-client/
git tag v0.1.0
git push origin v0.1.0
```

### Step 2: Update Service go.mod Files

Each service should import the library:

```go
require (
    github.com/Bengo-Hub/shared-service-client v0.1.0
)
```

**Note:** For local development, developers can use `go.work` at the `BengoBox` root to link all cloned repositories together.

### Step 3: Verify

```bash
# In any service repository
go mod tidy
go build ./cmd/api
```

## Local Development Setup

When developing locally, clone all repositories into a parent directory:

```bash
# Create parent directory
mkdir -p BengoBox
cd BengoBox/

# Clone all repositories
git clone https://github.com/Bengo-Hub/subscription-service.git subscription-service
git clone https://github.com/Bengo-Hub/logistics-service.git logistics-service
git clone https://github.com/Bengo-Hub/shared-service-client.git shared/service-client
# ... clone other services

# Create go.work at BengoBox root
cd BengoBox/
go work init \
  ./subscription-service \
  ./logistics-service \
  ./shared/service-client
```

This allows local development without needing to fetch from GitHub each time.

## Production Deployment

In production and CI/CD:

1. **Tagged versions** - always use specific versions (e.g., `v0.1.0`)
2. **Private module access** - configure `GOPRIVATE` and git credentials
3. **No replace directives** - services import directly from GitHub

## Versioning Strategy

- **v0.MAJOR.MINOR** for pre-1.0 releases
- **v1.0.0** for stable API
- Increment MAJOR for breaking changes
- Increment MINOR for new features (backward compatible)
- Increment PATCH for bug fixes

## Current Version

**v0.1.0** - Initial release with circuit breaker, retry, distributed tracing.

## Updating Services to Use New Versions

When releasing a new version:

1. **Tag the new version:**
   ```bash
   cd shared-service-client/
   git tag v0.2.0
   git push origin v0.2.0
   ```

2. **Update each service:**
   ```bash
   cd subscription-service/
   go get github.com/Bengo-Hub/shared-service-client@v0.2.0
   go mod tidy
   ```

3. **Test and deploy** each service independently

