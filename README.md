# ib-checker

Daily health checker for the Red Hat Image Builder service. Builds a matrix of RHEL versions and image types to verify the service is working correctly.

## How it works

1. Creates a temporary blueprint for each distribution/image-type combination
2. Triggers a compose from the blueprint
3. Polls for completion (up to 2 hours timeout)
4. Deletes the blueprint after completion
5. Reports results and sends notifications

All builds run in parallel using goroutines. The access token is automatically refreshed before expiry.

## Matrix

| Distribution | Image Types |
|-------------|-------------|
| rhel-8 | qcow2, aws, image-installer, vsphere-ova, wsl |
| rhel-9 | qcow2, aws, image-installer, vsphere-ova, wsl |
| rhel-10 | qcow2, aws, image-installer, vsphere-ova, wsl |

## Requirements

- podman
- just

## Usage

```bash
# Run linter
just lint

# Build binary
just build

# Run with a config file (requires credentials)
export CLIENT_ID="your-client-id"
export CLIENT_SECRET="your-client-secret"
just run config.yaml

# Or use an offline token
export OFFLINE_TOKEN="your-offline-token"
just run config.yaml

# Run smoke test
just run config.smoke.yaml
```

## Environment variables

| Variable | Required | Description |
|----------|----------|-------------|
| `OFFLINE_TOKEN` | Yes* | Red Hat API offline token |
| `CLIENT_ID` | Yes* | OAuth client ID for Red Hat SSO |
| `CLIENT_SECRET` | Yes* | OAuth client secret for Red Hat SSO |
| `SLACK_WEBHOOK` | No | Slack incoming webhook URL for notifications |
| `TELEGRAM_WEBHOOK` | No | Telegram bot webhook URL for notifications |
| `MESSAGE_PREFIX` | No | Prefix added to notifications (e.g., workflow URL) |

*Either `OFFLINE_TOKEN` or both `CLIENT_ID` and `CLIENT_SECRET` must be provided.

## Configuration

Edit `config.yaml` to customize the build matrix:

```yaml
matrix:
  distributions:
    - rhel-8
    - rhel-9
    - rhel-10
  image_types:
    - qcow2
    - aws
    - image-installer
    - vsphere-ova
    - wsl

aws_account_id: "123456789012"
```

The `aws_account_id` is used for sharing AWS AMIs when building `aws` image type.

## GitHub Actions

The repository includes two workflows:

- **Lint** (`lint.yml`) - Runs on pull requests and merge queue
- **Daily Check** (`daily.yml`) - Runs daily at 7am CET

### Required secrets

- `OFFLINE_TOKEN` or (`CLIENT_ID` and `CLIENT_SECRET`)
- `SLACK_WEBHOOK` (optional)
- `TELEGRAM_WEBHOOK` (optional)

## Notification format

```
- ✅ rhel-10/qcow2 5m30s
- ✅ rhel-9/aws 12m45s
- ❌ rhel-8/wsl 1h45m0s (compose failed)
```

## Exit code

- `0` - All builds succeeded
- `1` - One or more builds failed
