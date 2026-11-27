#!/bin/bash
# perf-only: one closed-loop eval of the current commit on the staging perf
# host. Starts the (stopped) host, deploys HEAD, sends jobs, collects each
# job's latency breakdown from ClickHouse, and stops the host again.
#
#   ops/loop/eval.sh                 # 3 fake-GitHub jobs on HEAD
#   EVAL_JOBS=5 KEEP_VM=1 ops/loop/eval.sh
#
# Environment:
#   AWS credentials/profile for the staging account (AWS_PROFILE, or the
#     GitHub workflow's OIDC role), AWS_REGION (default us-east-1)
#   EVAL_SSH_KEY     private key for the host (required)
#   EVAL_INSTANCE    instance id (default: the instance tagged
#                    Project=tachyon-perf, Name=tachyon-perf-staging)
#   EVAL_JOBS        jobs to send (3); EVAL_INTERVAL between them (20s)
#   EVAL_LABEL       runs-on label (tachyon-ubuntu-24.04-2x)
#   EVAL_SERIAL=1    send each job after the previous one is recorded
#                    (a host with room for one job at a time)
#   EVAL_GITHUB      fake (default) or real
#   EVAL_OUT         where results land (eval-out/)
#   KEEP_VM=1        leave the host running afterwards. On failure it is
#                    always left running for debugging, like kosha's bench.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
export AWS_REGION=${AWS_REGION:-us-east-1} AWS_PAGER=""
: "${EVAL_SSH_KEY:?set EVAL_SSH_KEY to the host private key}"
OUT=${EVAL_OUT:-eval-out}
mkdir -p "$OUT"
REF=$(git rev-parse --short HEAD)
BRANCH=$(git rev-parse --abbrev-ref HEAD)
log() { echo "eval: $*" >&2; }

ID=${EVAL_INSTANCE:-$(aws ec2 describe-instances \
	--filters Name=tag:Project,Values=tachyon-perf Name=tag:Name,Values=tachyon-perf-staging \
	Name=instance-state-name,Values=pending,running,stopping,stopped \
	--query 'Reservations[0].Instances[0].InstanceId' --output text)}
[ -n "$ID" ] && [ "$ID" != None ] || { log "no tachyon-perf-staging instance"; exit 1; }

state() { aws ec2 describe-instances --instance-ids "$ID" --query 'Reservations[0].Instances[0].State.Name' --output text; }
s=$(state)
if [ "$s" = stopping ]; then aws ec2 wait instance-stopped --instance-ids "$ID"; s=stopped; fi
if [ "$s" = stopped ]; then
	log "starting $ID"
	aws ec2 start-instances --instance-ids "$ID" >/dev/null
fi
aws ec2 wait instance-running --instance-ids "$ID"
IP=$(aws ec2 describe-instances --instance-ids "$ID" --query 'Reservations[0].Instances[0].PublicIpAddress' --output text)
log "host $ID at $IP"

ok=0 SG='' MYCIDR=''
# shellcheck disable=SC2317 # called by the EXIT trap
finish() {
	if [ -n "$SG" ]; then
		aws ec2 revoke-security-group-ingress --group-id "$SG" --ip-permissions \
			"IpProtocol=tcp,FromPort=22,ToPort=22,IpRanges=[{CidrIp=$MYCIDR}]" >/dev/null || true
	fi
	if [ "$ok" = 1 ] && [ -z "${KEEP_VM:-}" ]; then
		log "stopping $ID"
		aws ec2 stop-instances --instance-ids "$ID" >/dev/null
	else
		log "host left RUNNING for debugging: ssh -i $EVAL_SSH_KEY ubuntu@$IP (stop it with: aws ec2 stop-instances --instance-ids $ID)"
		[ -z "${GITHUB_STEP_SUMMARY:-}" ] || echo "**Host left running** ($ID, $IP) - stop it when done." >>"$GITHUB_STEP_SUMMARY"
	fi
}
trap finish EXIT

# EVAL_OPEN_SSH=1 (the workflow): allow SSH from this machine's address for
# this run only; the rule is removed on exit. Locally the security group
# already allows the operator's address.
if [ -n "${EVAL_OPEN_SSH:-}" ]; then
	SG=$(aws ec2 describe-instances --instance-ids "$ID" --query 'Reservations[0].Instances[0].SecurityGroups[0].GroupId' --output text)
	MYCIDR="$(curl -sf https://checkip.amazonaws.com | tr -d '[:space:]')/32"
	aws ec2 authorize-security-group-ingress --group-id "$SG" --ip-permissions \
		"IpProtocol=tcp,FromPort=22,ToPort=22,IpRanges=[{CidrIp=$MYCIDR,Description=perf-eval-run}]" >/dev/null
	log "opened SSH from $MYCIDR on $SG for this run"
fi

KH="$OUT/known_hosts"
SSH=(ssh -i "$EVAL_SSH_KEY" -o UserKnownHostsFile="$KH" -o StrictHostKeyChecking=accept-new -o ConnectTimeout=10 -o ServerAliveInterval=30 "ubuntu@$IP")
SCP=(scp -q -i "$EVAL_SSH_KEY" -o UserKnownHostsFile="$KH" -o StrictHostKeyChecking=accept-new)
for _ in $(seq 60); do "${SSH[@]}" -o BatchMode=yes true 2>/dev/null && break; sleep 5; done

log "deploying $BRANCH@$REF"
git archive --format=tar.gz -o "$OUT/src.tgz" HEAD
"${SCP[@]}" "$OUT/src.tgz" ops/loop/host/deploy.sh "ubuntu@$IP:/tmp/"
"${SSH[@]}" "sudo EVAL_GITHUB=${EVAL_GITHUB:-fake} bash /tmp/deploy.sh /tmp/src.tgz '$BRANCH@$REF'"

log "running ${EVAL_JOBS:-3} job(s)"
rc=0
"${SSH[@]}" "sudo TACHYON_CLICKHOUSE_PASSWORD=tachyon /opt/tachyon/src/bin/tachyon-eval run \
	-secret-file /opt/tachyon/secrets/webhook-secret -n ${EVAL_JOBS:-3} -interval ${EVAL_INTERVAL:-20s} \
	-label ${EVAL_LABEL:-tachyon-ubuntu-24.04-2x} ${EVAL_SERIAL:+-serial} -ref '$BRANCH@$REF' \
	-out /tmp/eval.json -summary /tmp/eval.md >/dev/null" || rc=$?
"${SCP[@]}" "ubuntu@$IP:/tmp/eval.json" "ubuntu@$IP:/tmp/eval.md" "$OUT/" || true
# Logs for the run, for debugging after the host is stopped.
"${SSH[@]}" "sudo journalctl -u tachyon-controlplane -u tachyon-hostd -u tachyon-fakegh --since '-30 min' -o cat --no-pager" >"$OUT/services.log" 2>/dev/null || true
[ -f "$OUT/eval.md" ] && cat "$OUT/eval.md"
[ -z "${GITHUB_STEP_SUMMARY:-}" ] || [ ! -f "$OUT/eval.md" ] || cat "$OUT/eval.md" >>"$GITHUB_STEP_SUMMARY"
[ "$rc" = 0 ] && ok=1
exit "$rc"
