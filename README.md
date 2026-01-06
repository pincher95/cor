## COR
****
COR (Cloud Orphan Resources) is a CLI tool to **find (and optionally delete) potentially orphaned AWS resources**.

### What it does

COR currently supports:

- **EBS volumes**: unattached volumes (`volumes`)
- **EBS snapshots**: snapshots not referenced by AMIs/volumes and not created by lifecycle policy (`snapshots`)
- **AMIs**: images not used by instances or launch templates (`images`)
- **Elastic IPs**: unassociated EIPs (`elasticips`)
- **ENIs**: unattached network interfaces (`enis`)
- **ELBv1**: classic ELBs with unhealthy instances (`elbv1`)
- **ELBv2**: application/network LBs with target groups that have no valid targets (`elbv2`)
- **ELBv2 target groups**: target groups not attached to any LB (`targetgroups`)
- **Auto Scaling Groups**: “empty” ASGs (no instances, min/desired=0, no LB/TG) (`autoscaling`)
- **NAT gateways**: list and optionally delete (`natgateways`)
- **RDS**: stopped instances + manual snapshots (`rds`)
- **CloudWatch Logs**: log groups (optionally delete) (`logs`)

### Install

From the repo:

```bash
go build -o cor .
./cor --help
```

### Authentication

Global flag: `--auth-method` (default: `AWS_CREDENTIALS_FILE`)

- **AWS_CREDENTIALS_FILE**: uses your standard AWS shared config/credentials files with `--profile`
- **ENV_SECRET**: uses `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`

### Configuration (file + env)

COR reads config from:

- `--config /path/to/file.yaml`, or
- `$HOME/.cor.yaml`

It also supports env vars prefixed with `COR_` (hyphens become underscores).

**Precedence**: CLI flags > config file > env vars > defaults

Example env vars:

- `COR_REGION=us-east-1`
- `COR_PROFILE=prod`
- `COR_AUTH_METHOD=AWS_CREDENTIALS_FILE`
- `COR_DELETE=true`
- `COR_SORT_BY=Name`
- `COR_SORT_DESC=true`

### Common flags

- `--region, -r`: AWS region (default: `us-east-1`)
- `--profile, -p`: shared config profile (default: `default`)
- `--auth-method, -a`: `AWS_CREDENTIALS_FILE` or `ENV_SECRET`
- `--delete`: actually delete/release resources (prompts for confirmation)
- `--sort-by`: sort output by a column name (buffers results; disables streaming)
- `--sort-desc`: descending sort
- `--config`: config file path (default: `$HOME/.cor.yaml`)

### Examples

List orphaned EBS volumes:

```bash
./cor volumes --region us-east-1 --profile default
```

Delete orphaned EBS volumes (will prompt):

```bash
./cor volumes --delete
```

List orphan AMIs, including those used by instances:

```bash
./cor images --include-used-by-instance
```

### Development

Run unit tests:

```bash
go test ./...
```

### Profiling (optional)

This binary can expose `pprof` endpoints **only when enabled**:

- `COR_PPROF=1` enables the server
- `COR_PPROF_ADDR=:6060` sets the bind address (default `:6060`)
