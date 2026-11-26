# Stage 6 moderated UX study

Use two disposable organizations and distinct single-role identities. Record the
build, browser, federation provider, role memberships and fixture IDs. Do not
record tokens, passwords or connection secrets. Start every task from a fresh
authenticated session. The moderator observes without coaching until the time
target; record assistance and continue only with participant consent.

| Role | Goal and starting point | Steps | Success criteria | Target |
|---|---|---|---|---|
| Developer | Portal home; own organization selected | Create project and service with Postgres; bind dev; inspect infrastructure card and connection outputs; request prod promotion | Dev becomes Ready, outputs usable by application, direct prod mutation denied | 15 min |
| Platform | Organization settings | Inspect environment configuration; try creating environment/dataplane; configure an allowed application secret | Protected objects denied; application configuration succeeds | 8 min |
| Approver | Pending change by another person | Review target and diff; approve; inspect status; reject a second change; attempt own approval | Independent approval recorded; self-approval denied; rejection visible | 5 min |
| Admin | Organization access page | Create restricted role binding; verify own access; attempt equivalent action in other organization | Own restricted binding works; cross-org modification denied | 8 min |
| Auditor | Portal home | Inspect services, changes, compliance and costs; look for write controls; attempt write by API | Authorized reads work; unauthorized writes denied | 5 min |
| Finance | Costs page | Select last month, project and service; inspect totals; attempt service access | Costs understandable; record actual extra access as a discrepancy | 5 min |
| Breakglass | Incident and emergency request | Request bounded access; second security on-call user approves; inspect Applied condition | Recorded/NotImplemented limitation explicit; no claim that access was granted | 8 min |
| Security | Compliance page | Inspect violation; request exception with expiry; review emergency audit evidence | Owner, expiry and approval trace understandable | 8 min |
| Datacenter operator | Machine inventory | Review unknown machine; request approval and decommission | No unapproved provisioning; storage risk visible | 10 min |

After each task ask: What was confusing? Did you know which organization and
environment you were affecting? What evidence told you the action succeeded?
Which denial was unexpected? For approvals: was the proposed production impact
clear? For costs: could you explain the allocation? For emergency access: was
the difference between recording and granting clear?

Rate 1 (strongly disagree) to 5 (strongly agree): I would use this frequently;
the system was unnecessarily complex; it was easy to use; I would need technical
help; functions were well integrated; there was too much inconsistency; most
people would learn quickly; it was cumbersome; I felt confident; I needed to
learn much before starting. Compute SUS: odd items minus 1, 5 minus even items,
sum times 2.5. Keep task-specific responses separate from SUS.

## Observation and results template

- Run/build/date:
- Participant pseudonym/role/prior experience:
- Organization and fixture IDs:
- Task/start/end/elapsed/target:
- Completed without help / with help / failed:
- Errors, retries and wrong-organization attempts:
- Exact confusing label and participant quotation:
- Moderator assistance and timestamp:
- Expected versus observed authorization:
- SUS responses and score:
- Task-specific responses:
- Severity, issue link, owner and retest date:
- Created objects, cleanup outcome and retained audit record IDs:

Report completion rate, median time and assistance rate per role. Preserve
individual failures; a mean score must not hide isolation or authorization
failures. Unsupported tasks are blocked, not passed.
