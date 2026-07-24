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
- **EFS**: file systems with zero mount targets (`efs`)
- **ECR**: untagged/old container images (`ecr`)
- **Route53**: hosted zones with only NS/SOA records (`route53zones`)
- **VPC Endpoints**: interface endpoints with no ENIs (`vpcendpoints`)
- **Client VPN**: endpoints with zero active connections (`clientvpn`)
- **Site-to-Site VPN**: connections with no tunnels up (`vpnconnections`)
- **Transit Gateway**: VPC attachments without route table association (`tgwattachments`)
- **Lambda Functions**: functions not invoked in 90+ days, old versions, unused provisioned concurrency (`lambda`)
- **ElastiCache**: clusters with zero connections for 24+ hours (`elasticache`)
- **OpenSearch**: domains with no indexing/search activity (`opensearch`)
- **DynamoDB Tables**: tables with zero read/write activity, empty tables (`dynamodb`)
- **S3 Buckets**: empty buckets, incomplete multipart uploads (`s3buckets`)
- **ECS Clusters**: clusters with no services or running tasks (`ecs`)
- **EC2 Instances**: instances stopped for too long, or running with near-zero CPU (`ec2`)
- **Bedrock**: idle provisioned-throughput model units (`bedrock`)
- **IAM Roles**: customer-managed roles unused for 90+ days, excluding AWS service-linked roles (`iamroles`)
- **IAM Policies**: customer-managed policies attached to no principal and used as no permissions boundary (`iampolicies`)

### Cost Impact

COR helps identify resources that continue to incur AWS charges even when orphaned or unused:

- **EBS Volumes**: ~$0.08-0.10/GB-month (gp3), ~$0.10/GB-month (gp2)
- **EBS Snapshots**: ~$0.05/GB-month
- **AMIs**: Includes underlying snapshot costs
- **Elastic IPs**: $0.005/hour when unassociated (~$3.60/month)
- **NAT Gateways**: ~$0.045/hour + data transfer (~$32/month minimum)
- **ALB/NLB**: ~$0.0225/hour (~$16/month per load balancer)
- **Classic ELB**: ~$0.025/hour (~$18/month)
- **RDS Instances**: Varies by instance type (even when stopped, snapshots incur costs)
- **RDS Snapshots**: ~$0.095/GB-month
- **CloudWatch Logs**: $0.50/GB ingestion + $0.03/GB-month storage
- **EFS**: $0.30/GB-month (Standard), $0.016/GB-month (Infrequent Access)
- **ECR**: $0.10/GB-month for storage
- **Route53 Hosted Zones**: $0.50/month per zone
- **VPC Endpoints**: ~$0.01/hour per AZ (~$7/month)
- **Client VPN**: ~$0.10/hour (~$72/month)
- **Site-to-Site VPN**: ~$0.05/hour per connection (~$36/month)
- **Lambda Functions**: Provisioned concurrency $100-500/month per function; storage for old versions
- **ElastiCache**: cache.m5.large ~$105/month, cache.r6g.xlarge ~$217/month
- **OpenSearch**: t3.small.search ~$26/month, r6g.large.search ~$101/month + $0.135/GB-month storage
- **DynamoDB Tables**: Provisioned mode $0.00065/hour per WCU, $0.00013/hour per RCU; storage $0.25/GB-month
- **S3 Buckets**: Storage $0.023/GB-month (Standard); request costs; incomplete uploads consume storage
- **ECS Clusters**: Free, but prevents cleanup of NAT Gateway, ALB, and related infrastructure
- **IAM Roles / Policies**: No direct charge — cleanup reduces attack surface and reclaims account entity quotas (default 1,000 roles / 1,500 customer-managed policies)

*Note: Prices are approximate and vary by region. Check current AWS pricing for your region.*

### Install

From the repo:

```bash
make build          # produces ./cor
./cor --help
```

`make build` injects `version`/`commit`/`buildDate` from `git` via `-ldflags`.
Plain `go build -o cor .` also works.

Run `make help` for the full target list (test, lint, cover, dist, …).

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
- `COR_YES=true`
- `COR_ON_ERROR=continue`
- `COR_DRY_RUN=true`
- `COR_FORMAT=json`
- `COR_LOG_FORMAT=json`
- `COR_METRICS_FILE=/var/log/cor.ndjson`
- `COR_STATE_FILE=/var/lib/cor/state`
- `COR_SORT_BY=Name`
- `COR_SORT_DESC=true`

### Common flags

- `--region, -r`: AWS region (default: `us-east-1`)
- `--profile, -p`: shared config profile (default: `default`)
- `--auth-method, -a`: `AWS_CREDENTIALS_FILE` or `ENV_SECRET`
- `--delete`: actually delete/release resources (prompts for confirmation)
- `--yes`: skip the delete confirmation prompt — use only in automation
- `--dry-run`: print what would be deleted without calling AWS delete APIs
- `--on-error stop|continue`: per-item delete-error policy (default `stop`)
- `--state-file <path>`: persist deleted-resource keys; skip them on re-run
- `--sort-by`: sort output by a column name (buffers results; disables streaming)
- `--sort-desc`: descending sort
- `--format table|json|csv`: output format (default `table`)
- `--log-format text|json`: log line format (default `text`)
- `--metrics-file <path>`: append a per-run JSON metrics summary to this path
- `--timeout`: overall command timeout (e.g. `5m`)
- `--config`: config file path (default: `$HOME/.cor.yaml`)
- `--all-regions`: scan every enabled region for the account (one unified table)
- `--min-cost <usd>`: only show orphans estimated at or above this monthly cost
- `--top-n <n>`: show only the N most expensive orphans
- `--rank`: add a Rank column ordered by estimated monthly cost
- `--save-baseline <path>` / `--diff-baseline <path>`: snapshot orphans+costs, then diff a later run
- `--with-ce`: prorate snapshot/AMI/RDS estimates against actual Cost Explorer spend (**$0.01 per call**)

### Examples

**List orphaned EBS volumes:**

```bash
./cor volumes --region us-east-1 --profile default
```

**Delete orphaned EBS volumes (will prompt):**

```bash
./cor volumes --delete
```

**Filter ELB by name pattern:**

```bash
./cor elbv1 --filter-by-name "prod-*" --show-tags
```

**Filter resources by tags:**

```bash
./cor volumes --filter-by-tags "Environment=dev,Owner=team-a"
./cor elbv2 --filter-by-tags "CostCenter=engineering"
```

**Sort output for easier analysis:**

```bash
./cor snapshots --sort-by "Size" --sort-desc
./cor volumes --sort-by "CreateTime"
```

**Use config file for consistent settings:**

```bash
./cor --config ./prod-config.yaml volumes
```

**Preview deletes without calling AWS (`--dry-run`):**

```bash
./cor volumes --delete --dry-run
```

**Non-interactive delete for automation (`--yes`):**

```bash
./cor volumes --delete --yes --on-error continue
```

**Machine-readable output (`--format`):**

```bash
./cor volumes --format json
./cor snapshots --format csv --sort-by Size --sort-desc > orphans.csv
```

**Resumable runs with a state file:**

```bash
# first run records every deleted ID; interrupt at any time
./cor volumes --delete --yes --state-file /tmp/cor-volumes.state
# re-run skips already-deleted IDs
./cor volumes --delete --yes --state-file /tmp/cor-volumes.state
```

**Per-run metrics + JSON logs (good for CI / cron):**

```bash
./cor volumes --log-format json --metrics-file /var/log/cor-metrics.ndjson
```

**List orphan AMIs, including those used by instances:**

```bash
./cor images --include-used-by-instance
```

**Check ECR for old/untagged images (30+ days):**

```bash
./cor ecr --days-old 30
```

**Find Route53 zones with only NS/SOA records:**

```bash
./cor route53zones
```

**List RDS instances and manual snapshots:**

```bash
./cor rds
```

**Find Lambda functions not invoked in 90+ days:**

```bash
./cor lambda
./cor lambda --days-since-invocation 180  # Custom threshold
```

**Find idle ElastiCache clusters:**

```bash
./cor elasticache
./cor elasticache --hours-zero-connections 48  # Zero connections for 48+ hours
```

**Find orphaned OpenSearch domains:**

```bash
./cor opensearch
./cor opensearch --days-no-indexing 14 --hours-no-searches 48  # Custom thresholds
```

**Find idle DynamoDB tables:**

```bash
./cor dynamodb
./cor dynamodb --days-no-activity 60  # No activity for 60+ days
```

**Find orphaned S3 buckets:**

```bash
./cor s3buckets
./cor s3buckets --check-lifecycle  # Also check for missing lifecycle policies
```

**Find empty ECS clusters:**

```bash
./cor ecs
```

**Cost analysis (`cost` / `pricing`):**

Every orphan command shows an estimated monthly cost per row. Two extra commands
work on the cost dimension itself:

```bash
# total estimated monthly cost of every orphan type, in one table
./cor cost
./cor cost --all-regions

# rank the most expensive orphans of one type
./cor snapshots --rank --top-n 20
./cor volumes --min-cost 25

# inspect the rate table in effect (bundled defaults, or a refreshed cache)
./cor pricing show
./cor pricing show eu-west-1

# fetch live prices from the AWS Pricing API into ~/.cor/prices/<region>.json
./cor pricing refresh
./cor pricing refresh --all-regions
```

Estimates use bundled **us-east-1 list prices** (no Reserved Instance or Savings
Plan discounts). A region without a refreshed cache silently falls back to
us-east-1 rates, so run `cor pricing refresh` for accurate non-us-east-1 numbers.
A cost of `—` means the rate is unknown, not that the resource is free.

**Track cost drift between runs:**

```bash
./cor volumes --save-baseline /tmp/volumes-week1.ndjson
# ... a week later
./cor volumes --diff-baseline /tmp/volumes-week1.ndjson   # added / removed / changed
```

**IAM hygiene (security, not cost):**

`iamroles` and `iampolicies` are account-global (they run once even with
`--all-regions`) and have no `Est $/mo` column — they exist to shrink attack
surface and reclaim IAM entity quotas. Always audit first; IAM deletion is
irreversible.

```bash
# unused roles > 180 days, audit only (JSON for review)
./cor iamroles --max-unused-days 180 --format json

# also flag roles with no attached/inline policies and no instance profile
./cor iamroles --include-empty

# unattached customer-managed policies, preview deletion without touching AWS
./cor iampolicies --delete --dry-run

# clean up unused app roles under a path, non-interactive + resumable
./cor iamroles --path-prefix /application/ --delete --yes \
    --state-file /var/lib/cor/iamroles.state --on-error continue

# safety allowlist: only touch entities you explicitly tagged
./cor iampolicies --require-tag cor-managed=true --delete --dry-run
```

AWS service-linked roles (`/aws-service-role/…`) and AWS-managed policies are
always excluded and never deleted.

`--require-tag key[=value]` (comma-separated for multiple) is an opt-in
allowlist: when set, only entities carrying **all** the given tags are eligible —
anything else is not even listed. Recommended for shared accounts to restrict
COR to entities you own (e.g. `cor-managed=true`).

**Required IAM permissions.** Read-only (listing) needs:

```
iam:ListRoles, iam:GetRole, iam:ListAttachedRolePolicies, iam:ListRolePolicies,
iam:ListInstanceProfilesForRole, iam:ListPolicies, iam:ListPolicyVersions,
iam:ListPolicyTags
```

(`iam:ListPolicyTags` is only needed when using `--require-tag` on `iampolicies`;
role tags come back with `iam:GetRole`.)

`--delete` additionally needs:

```
iam:DetachRolePolicy, iam:DeleteRolePolicy, iam:RemoveRoleFromInstanceProfile,
iam:DeleteRole, iam:DeletePolicyVersion, iam:DeletePolicy
```

### Troubleshooting

**Permission Denied Errors**

- Ensure your IAM user/role has read permissions for the resource type you're checking
- For delete operations, write/delete permissions are required
- Common required permissions: `ec2:Describe*`, `elasticloadbalancing:Describe*`, `rds:Describe*`, etc.
- Use AWS IAM Policy Simulator to verify permissions

**Timeout Issues**

- Use the `--timeout` flag to increase execution time for large environments
- Consider filtering by region or resource tags to reduce scope
- Some commands (like snapshots, images) can take longer in accounts with many resources

**No Resources Found**

- Verify you're using the correct region with `--region` or `-r`
- Check that resource state filters match your use case (e.g., volumes must be in 'available' state)
- Ensure your credentials have permission to view the resources
- Try running with `--profile` to verify you're using the correct AWS account

**Rate Limiting / Throttling**

- AWS APIs have rate limits; COR implements retries with exponential backoff
- If you consistently hit limits, consider running during off-peak hours
- Use filtering options to reduce the number of API calls

**Memory Issues**

- COR uses streaming output by default to keep memory usage low
- Avoid using `--sort-by` for very large result sets (requires buffering all results)
- If issues persist, check for resource leaks or file an issue on GitHub

### Development

The repo ships a `Makefile` covering the common workflows:

```bash
make build              # compile ./cor with ldflags-injected version
make test               # go test ./...
make test-race          # go test -race ./...
make cover              # write coverage.out + print the summary
make lint               # golangci-lint run ./...
make vet                # go vet ./...
make fmt                # gofmt -s -w .
make tidy               # go mod tidy
make vendor             # go mod vendor
make run ARGS='volumes --region us-east-1'
make dist               # cross-compile linux+darwin x amd64+arm64 into ./dist
make clean              # remove ./cor, coverage.out, dist/
make help               # list all targets
```

Architecture, command pattern, and contributor notes live in `CLAUDE.md`,
`CONTRIBUTING.md`, and (for deeper backgrounder) `research.md`.

### Profiling (optional)

This binary can expose `pprof` endpoints **only when enabled**:

- `COR_PPROF=1` enables the server
- `COR_PPROF_ADDR=:6060` sets the bind address (default `:6060`)

### License

Apache-2.0 — see `LICENSE` and `NOTICE`.
