# Setup Guide for Shared Service Client

## Repository Structure

**Important:** `shared-service-client` is an **independent GitHub repository** that must be created in the `Bengo-Hub` organization.

**Repository URL:** `https://github.com/Bengo-Hub/shared-service-client`

## Initial Setup Steps

### Step 1: Create GitHub Repository

1. Go to the Bengo-Hub organization on GitHub
2. Create a new repository named `shared-service-client`
3. Make it private (recommended) or public
4. **Do not** initialize with README, .gitignore, or license (we already have these)

### Step 2: Push Code to Repository

```bash
# In the shared/service-client directory
cd shared/service-client

# Initialize git if not already done
git init
git add .
git commit -m "Initial commit: shared-service-client v0.1.0"

# Add remote and push
git remote add origin https://github.com/Bengo-Hub/shared-service-client.git
git branch -M main
git push -u origin main
```

### Step 3: Tag the First Version

```bash
# Tag v0.1.0
git tag v0.1.0
git push origin v0.1.0
```

### Step 4: Update Service Repositories

Once the repository is created and tagged, update each service to use it:

```bash
# In each service directory
go get github.com/Bengo-Hub/shared-service-client@v0.1.0
go mod tidy
```

## Local Development Workflow

### Using Go Workspace (Recommended)

1. Clone all repositories into a parent directory:
```bash
mkdir -p BengoBox
cd BengoBox/

git clone https://github.com/Bengo-Hub/shared-service-client.git shared/service-client
git clone https://github.com/Bengo-Hub/subscription-service.git subscription-service
# ... clone other services
```

2. Create `go.work` at BengoBox root:
```bash
cd BengoBox/
go work init \
  ./subscription-service \
  ./logistics-service \
  ./ordering-service/ordering-backend \
  ./shared/service-client
```

This allows local development without needing to fetch from GitHub each time.

## Versioning Strategy

- **v0.MAJOR.MINOR** for pre-1.0 releases
- **v1.0.0** for stable API
- Increment MAJOR for breaking changes
- Increment MINOR for new features (backward compatible)
- Increment PATCH for bug fixes

## Current Version

**v0.1.0** - Initial release with circuit breaker, retry, distributed tracing, and structured logging.

