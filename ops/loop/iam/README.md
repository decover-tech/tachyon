# IAM role for the perf eval workflow (perf-only)

`.github/workflows/perf-eval.yml` assumes this role through GitHub OIDC. It
can only be assumed by workflows on `decover-tech/tachyon`'s
`perf/instrumentation` branch, and it can only start and stop instances
tagged `Project=tachyon-perf` and open SSH on that project's security group
for the duration of a run. An account admin creates it once:

```sh
# Once per account, if GitHub's OIDC provider is not there yet:
aws iam create-open-id-connect-provider \
  --url https://token.actions.githubusercontent.com --client-id-list sts.amazonaws.com

aws iam create-role --role-name tachyon-perf-eval \
  --assume-role-policy-document file://ops/loop/iam/trust-policy.json
aws iam put-role-policy --role-name tachyon-perf-eval --policy-name tachyon-perf-eval \
  --policy-document file://ops/loop/iam/permissions-policy.json
```

Then set the repository secrets `TACHYON_PERF_ROLE_ARN`
(`arn:aws:iam::010928200670:role/tachyon-perf-eval`) and
`TACHYON_PERF_SSH_KEY` (the host's private key).
