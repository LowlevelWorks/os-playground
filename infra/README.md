# Infrastructure

Everything the playground runs on, as Terraform. Changes go through pull
requests: CI plans them, a reviewer reads the plan, merging applies it.

```
Browser ─HTTPS/WSS─▶ CloudFront + WAF  (os.example.com)
                      ├── /*      ─OAC──────────▶ S3 site bucket (private)
                      └── /api/*  ─VPC origin───▶ internal ALB :443 (private subnets)
                                                    └─▶ Auto Scaling group, ≥2 hosts in different AZs
                                                          Docker: API container (read-only, no caps) → QEMU/KVM guests
Hosts egress: NAT gateway per AZ │ S3 gateway endpoint │ IMDSv2 hop limit 1
```

Nothing in the VPC has a public address. The ALB is internal and CloudFront
reaches it through a VPC origin, so there is no origin to discover or bypass.

## Layout

| Path | What | State |
|---|---|---|
| `bootstrap/` | The S3 bucket holding all Terraform state | local (run once) |
| `global/` | Route 53 zone, ECR, OS image bucket, CI roles, budget | `global/terraform.tfstate` |
| `envs/staging/`, `envs/prod/` | One complete environment each; differ only in `terraform.tfvars` | `envs/<env>/terraform.tfstate` |
| `modules/environment` | Composes the modules below, plus certificates, alarm topic and the environment's deploy role | |
| `modules/network` | VPC, subnets in 2–3 AZs, NAT per AZ, S3 endpoint, flow logs | |
| `modules/api` | Internal ALB, launch template, Auto Scaling group, host role, alarms | |
| `modules/edge` | CloudFront, VPC origin, site bucket, WAF, DNS records | |

State is locked with S3 lock files (`use_lockfile`), so no DynamoDB table.

## How a release works

Infrastructure and releases are separate on purpose:

- **Terraform** owns the launch template, the group and everything around them.
  A launch template change (new AMI, instance type, user data) rolls the hosts
  through the group's `instance_refresh` block.
- **The deploy workflow** owns *which image runs*. It writes the image to the
  SSM parameter `/osp-<env>/api/image` and starts an instance refresh. Hosts
  read the parameter when they boot. Terraform ignores the parameter's value.

On a merge to `main`, CI builds the image once (tagged with the commit SHA),
deploys it and the site to **staging**, smoke-tests it, then waits for approval
before shipping the **same image and files** to **prod**. A failed refresh or
smoke test puts the previous image back and rolls the hosts onto it.

## Rough monthly cost (Sydney, USD)

| | staging | prod |
|---|---|---|
| NAT gateways | 1 × ~$43 | 3 × ~$43 |
| Internal ALB | ~$25 | ~$25 |
| c8i.large hosts | 1 × ~$90 | 2 × ~$90 |
| WAF (ACL + 4 rules + requests) | ~$10 | ~$10 |
| CloudWatch, flow logs, S3, CloudFront | ~$10 | ~$20 |
| **About** | **~$180** | **~$380** |

Check current prices; these are estimates. Staging can be paused by setting
`desired_capacity` and `min_size` to 0 (alarms will fire; that is expected).

## First-time setup

Run these from a workstation with administrator credentials (`aws login`),
Terraform ≥ 1.10, in this order.

### 0. Prerequisite: the GitHub OIDC provider

This account's organization has a service control policy that **denies the
IAM OpenID Connect provider APIs**. GitHub Actions authenticates to AWS
through that provider, so:

- Ask the organization's administrator to create it (URL
  `https://token.actions.githubusercontent.com`, audience `sts.amazonaws.com`)
  or to exempt this account, **or**
- if the policy is lifted, set `manage_github_oidc_provider = true` in
  `global/terraform.tfvars` and Terraform creates it.

Without it, the CI roles exist but nothing can assume them.

### 1. Fill in the TODOs

- `global/terraform.tfvars`: `zone_name` (e.g. `os.yourdomain.com`), budget email.
- `envs/*/terraform.tfvars`: `hostname` (prod: the zone apex; staging:
  `staging.<zone>`), alarm email, optionally `allowed_ip_cidrs` for staging.

### 2. State bucket

```sh
terraform -chdir=infra/bootstrap init
terraform -chdir=infra/bootstrap apply
```

### 3. Global stack, then delegate DNS

```sh
terraform -chdir=infra/global init
terraform -chdir=infra/global apply      # also adopts the existing ECR repo and assets bucket
terraform -chdir=infra/global output zone_name_servers
```

At your domain's DNS provider, add an **NS** record for the zone name (e.g.
`os`) with those four name servers. Check with `dig NS os.yourdomain.com`
before continuing: certificate validation in step 4 waits on it.

### 4. Environments

```sh
terraform -chdir=infra/envs/staging init && terraform -chdir=infra/envs/staging apply
terraform -chdir=infra/envs/prod    init && terraform -chdir=infra/envs/prod    apply
```

The first apply takes 20–30 minutes (certificates, CloudFront, VPC origin).
Confirm the SNS subscription emails.

### 5. GitHub

**Environments** (Settings → Environments):

| Environment | Deployment branches | Required reviewers | Variables |
|---|---|---|---|
| `staging` | `main` | none | `terraform -chdir=infra/envs/staging output github_environment_variables` |
| `prod` | `main` | you | same, from `envs/prod` |
| `infrastructure` | `main` | you | none |

**Repository variables** (Settings → Secrets and variables → Actions →
Variables): the three values from `terraform -chdir=infra/global output github_role_arns`.

None of these are secrets: a role ARN is useless without a GitHub token whose
subject matches the role's trust policy.

### 6. Upload the site once

The first CI run uploads it; until then the site bucket is empty. Merge any
change under `apps/web`, or run the two `aws s3 sync` commands from
`.github/workflows/deploy.yml` by hand.

### 7. Retire the hand-built environment

Once prod serves on the new hostname, delete what was built in the console:
the old CloudFront distribution (`E2GEQULPWCXKUD`, disable first), its
`os-playground-web` bucket, the `os-playground-sg` Auto Scaling group, its
launch template, the `os-playground-alb` load balancer and target group, the
NAT gateway (and release its Elastic IP), then the `os-playground-prod-vpc`
VPC. Also check `CreatedByCloudFront-*` web ACLs in us-east-1 (WAF, billed
monthly) and the `os-playground-api-role` IAM role.

## Operations

| Task | How |
|---|---|
| Shell on a host | EC2 → instance → Connect → Session Manager. There is no SSH. |
| API logs | CloudWatch Logs `/osp-<env>/api`, one stream per instance |
| Boot (user data) log | EC2 → instance → Actions → Monitor and troubleshoot → Get system log |
| Roll back a release | Re-run the deploy of an older commit, or set `/osp-<env>/api/image` to an older tag and start an instance refresh |
| Patch the OS | Any plan after AWS publishes a new Amazon Linux AMI shows a launch template change; applying it rolls the hosts |
| Replacement loop | Group → Advanced configurations → suspend `ReplaceUnhealthy`, read the system log, fix, resume |
| Pause staging | `min_size = 0`, `desired_capacity = 0` in `envs/staging/terraform.tfvars` |

## Design notes and limits

- **Sessions live in host memory.** The target group uses ALB cookie
  stickiness so a browser keeps reaching the host that runs its VM. Limits
  (`MAX_SESSIONS`, per-client caps, rate limits) are per host. A host going
  away ends the VMs on it; deploys drain hosts for 5 minutes first.
- **Guests run untrusted code.** They have no network (`-nic none`), run under
  KVM in a container without capabilities or a writable root, and the
  container cannot reach the instance's credentials (metadata hop limit 1).
- **Staging behind an IP allow-list** blocks the deploy smoke test from GitHub's
  runners. Either keep staging public or drop the smoke test for staging.
- **Not yet here**, in rough priority order: CloudFront access logs, a
  CloudFront 5xx alarm (metrics live in us-east-1), a baked AMI (Image Builder)
  instead of installing Docker at boot, a CloudWatch dashboard, and moving
  staging to its own AWS account.
