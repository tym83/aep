# Infrastructure RCA evaluation

`cases.yaml` defines 30 failures, five each for database, volume, node, quota,
network and release. Each has injection and cleanup instructions, an expected
cause, a symptom and a case-insensitive scoring expression. The runner never
executes the YAML shell snippets. They are a manual stand specification; 28
injectors remain TODO. Only `quota-pods` and `database-disk-full` run automatically.
The latter is explicitly simulated and never writes to a database disk.

## Stand contract

Use a disposable organization with a dedicated non-production tier and no customer
workloads. The real quota injector temporarily sets the existing tier quota's
`spec.hard.pods` to zero and creates a disposable Deployment, whose ReplicaSet
produces FailedCreate quota events. Existing pods are not deleted; all new pod
creation in that tier is blocked until cleanup. Use an unscoped ResourceQuota.
The quota must not be concurrently reconciled by another controller during a run.

The simulation changes only `aep.aenix.io/eval-simulated-failure` on an existing
Postgres app with an annotations map. It does not create disk pressure or realistic
telemetry and cannot establish RCA quality. Both injectors restore the original
value using JSON Patch tests; concurrent edits cause a recovery error instead of
being overwritten. A journal is created with mode 0600 and fsynced before mutation.
SIGINT, SIGTERM, failures and timeouts attempt cleanup. After SIGKILL or a machine
failure, use `--restore`. Retain and inspect the journal after any cleanup error;
manual reconciliation is required if the object was replaced or edited.

For manual snippets export ORG, TIER (never prod), NS=tenant-$ORG-$TIER,
GUEST_CONTEXT, APP_NS, PROJECT, COMPONENT, APP, WORKLOAD, CONTAINER, SERVICE,
RELEASE, QUOTA, TIER_WORKLOAD, STAND_NODE, STANDBY_POD, RWO_WORKLOAD,
STAND_STORAGE_CLASS, IMAGE and SNAPSHOT (a unique local snapshot path). These
identify existing disposable fixtures, not arbitrary production resources.
Use one case at a time and restore before the next case. Save Deployment history
and ensure only one rollout occurs, so `rollout undo` restores the intended revision.
For quota storage/PVC cases the fixture action must create a new PVC, not just a
pod. The database fixture provides a table `aep_eval`, credentials already inside
LOAD_POD, pgbench/psql, and a connection limit below 150. Lock/connection load is
bounded to 120 seconds. The volume fixtures require a data mount and the relevant
StorageClass. The RWO fixture requires two nodes and hard hostname anti-affinity.
NetworkPolicy cases require enforcement and no other policy allowing that traffic.
The release chart case uses a standalone HelmRelease with spec.chart.spec.version.
Do not pause platform controllers or experiment on physical storage hosts.

## Running

Build with `go build -o ./rca-eval ./cmd/rca-eval`. List cases without credentials
or Kubernetes access:

```sh
./rca-eval --suite eval/cases.yaml
```

Set `AEP_EVAL_TOKEN` to the organization's agent/report API token without saving it
in shell history. Use the per-organization agent URL through the authorized portal
path or a local port-forward. The runner POSTs the observer-shaped alert directly
to `/api/v1alpha1/rca-agent/analyze`, reads `report_id`, and polls
`/api/v1/rca-agent/reports/{report_id}` until `completed`, `failed` or the deadline.
The NetworkPolicy intentionally does not permit a general eval pod to contact the
agent; run through a local port-forward on the stand. The router is tested separately.

```sh
./rca-eval --execute --case quota-pods --org example --tier nonprod \
  --quota tenant-quota --project stand --component probe --environment dev \
  --agent-url http://127.0.0.1:8080 --kubeconfig ./stand.kubeconfig \
  --journal ./eval-recovery.json > quota-result.json

./rca-eval --execute --case database-disk-full --org example --tier nonprod \
  --app stand-db --project stand --component probe --environment dev \
  --agent-url http://127.0.0.1:8080 --kubeconfig ./stand.kubeconfig \
  --journal ./eval-recovery.json > simulation-result.json

./rca-eval --restore --kubeconfig ./stand.kubeconfig --journal ./eval-recovery.json
```

Recovery is only needed when a journal remains. The default deadline is ten minutes,
poll interval five seconds, evidence settling period fifteen seconds. Override
`--workload-image` with an approved mirrored image on disconnected stands.
The container image includes the suite at `/eval/cases.yaml`; mount a writable
persistent directory for `--journal` so recovery survives container deletion.

## Score and interpretation

The first root cause's `summary` and `analysis` are matched against the case regex.
Other ranked causes, recommendations, echoed alert text and top-level summary do
not count. Expected answers, case IDs and scoring regexes are not sent to the agent.
This is a deterministic lexical score, not semantic judging: inspect false matches
and review regex changes with the failure specification.

Output is JSON: per-case status, report ID, correctness, time to report, simulation
marker and errors; aggregate top-1 accuracy, suite coverage and mean report time.
Time starts before injection and includes settling and polling; failed cases count
as incorrect, while simulations and TODO cases are excluded. Coverage divides
eligible cases by 30. Acceptance requires all 30 real cases and accuracy >= 0.70;
this implementation cannot claim acceptance with one real injector. Timeout and
cleanup failures exit nonzero. Unsupported cases are reported as `stubbed`.
Cost measurement is deferred until the chosen inference endpoint provides usage.

To add an injector implement `rcaeval.Injector`, register it in the command, save
recovery state before mutation, and test normal cleanup, partial failure, timeout
and concurrent edits. No injector may accept a target outside the selected stand.
