import { useEffect, useState, useId } from 'react';
import { useApi } from '@backstage/core-plugin-api';
import { useEntity } from '@backstage/plugin-catalog-react';
import { Content, Header, Page, InfoCard } from '@backstage/core-components';
import {
  Button,
  Dialog,
  DialogTitle,
  DialogContent,
  DialogActions,
  TextField,
  MenuItem,
  Typography,
} from '@material-ui/core';
import {
  aepApiRef,
  Change,
  ChangeInput,
  Service,
  Session,
  serviceState,
} from './api';

function useSession() {
  const api = useApi(aepApiRef);
  const [session, setSession] = useState<Session>();
  const [org, setOrg] = useState('');
  const [error, setError] = useState('');
  useEffect(() => {
    api
      .session()
      .then(s => {
        setSession(s);
        setOrg(s.organizations[0]?.name ?? '');
      })
      .catch(e => setError(e.message));
  }, [api]);
  return { session, org, setOrg, error };
}
function OrgSelect({
  session,
  org,
  onChange,
}: {
  session?: Session;
  org: string;
  onChange: (value: string) => void;
}) {
  return (
    <TextField
      select
      label="Organization"
      value={org}
      onChange={e => onChange(e.target.value)}
    >
      {session?.organizations.map(o => (
        <MenuItem key={o.name} value={o.name}>
          {o.name}
        </MenuItem>
      ))}
    </TextField>
  );
}
function ErrorMessage({ error }: { error: string }) {
  return error ? (
    <Typography role="alert" color="error">
      {error}
    </Typography>
  ) : null;
}
export function ServiceSummary({ service }: { service: Service }) {
  const state = serviceState(service);
  const url = /^https?:\/\//.test(service.application.consoleUrl)
    ? service.application.consoleUrl
    : undefined;
  return (
    <section>
      <h3>
        {['resource', 'project', 'environment']
          .map(k => service.labels[`aep.aenix.io/${k}`])
          .filter(Boolean)
          .join(' · ') || service.name}
      </h3>
      <p>
        {service.kind} — {state.phase}: {state.reason}
      </p>
      <p>
        Application: {service.application.namespace}/{service.application.name}
      </p>
      {url && (
        <a href={url} target="_blank" rel="noopener noreferrer">
          Open Cozystack console
        </a>
      )}
    </section>
  );
}
export function InfrastructurePage() {
  const api = useApi(aepApiRef);
  const ctx = useSession();
  const [services, setServices] = useState<Service[]>();
  const [error, setError] = useState('');
  useEffect(() => {
    let active = true;
    setServices(undefined);
    setError('');
    if (ctx.org)
      api
        .services(ctx.org)
        .then(r => {
          if (active) setServices(r.items);
        })
        .catch(e => {
          if (active) setError(e.message);
        });
    return () => {
      active = false;
    };
  }, [api, ctx.org]);
  return (
    <Page themeId="tool">
      <Header title="Organization infrastructure" />
      <Content>
        <OrgSelect session={ctx.session} org={ctx.org} onChange={ctx.setOrg} />
        <ErrorMessage error={ctx.error || error} />
        {services ? (
          services.map(s => <ServiceSummary key={s.name} service={s} />)
        ) : (
          <p>Loading infrastructure…</p>
        )}
        {services?.length === 0 && (
          <p>No bridge-managed services or clusters.</p>
        )}
      </Content>
    </Page>
  );
}
export function RequestChange({
  organization,
  target,
  operation = 'promote',
}: {
  organization: string;
  target: ChangeInput['target'];
  operation?: string;
}) {
  const fieldId = useId();
  const api = useApi(aepApiRef);
  const [open, setOpen] = useState(false);
  const [justification, setJustification] = useState('');
  const [desired, setDesired] = useState('{}');
  const [environment, setEnvironment] = useState(target.environment ?? '');
  const [op, setOp] = useState(operation);
  const [key, setKey] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [result, setResult] = useState('');
  const changed = () => setKey(crypto.randomUUID());
  async function submit() {
    setBusy(true);
    setError('');
    try {
      const value = JSON.parse(desired);
      if (!value || Array.isArray(value) || typeof value !== 'object')
        throw new Error('Desired state must be a JSON object');
      const response = await api.create(
        {
          organization,
          operation: op,
          model: 'normal',
          target: { ...target, environment },
          desired: value,
          justification,
        },
        key,
      );
      setResult(`Change requested: ${response.id}`);
      setOpen(false);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  const label =
    (
      {
        'resource-restore': 'Restore',
        'credential-rotate': 'Rotate password',
        'resource-update': 'Resize',
      } as Record<string, string>
    )[operation] ?? 'Request change';
  return (
    <>
      <Button
        onClick={() => {
          changed();
          setOpen(true);
        }}
        disabled={!organization}
      >
        {label}
      </Button>
      {result && <p role="status">{result}</p>}
      <Dialog
        open={open}
        onClose={() => {
          if (!busy) setOpen(false);
        }}
        fullWidth
      >
        <DialogTitle>Request change</DialogTitle>
        <DialogContent>
          <p>
            {target.kind}: {target.name}
          </p>
          <TextField
            select
            fullWidth
            id={`${fieldId}-operation`}
            label="Operation"
            value={op}
            onChange={e => {
              setOp(e.target.value);
              changed();
            }}
          >
            {[
              'promote',
              'rollback',
              'resource-update',
              'resource-restore',
              'credential-rotate',
            ]
              .filter(
                v =>
                  target.kind === 'Resource' ||
                  ['promote', 'rollback'].includes(v),
              )
              .map(v => (
                <MenuItem key={v} value={v}>
                  {v}
                </MenuItem>
              ))}
          </TextField>
          <TextField
            id={`${fieldId}-environment`}
            label="Environment"
            fullWidth
            value={environment}
            onChange={e => {
              setEnvironment(e.target.value);
              changed();
            }}
          />
          <TextField
            id={`${fieldId}-justification`}
            label="Justification"
            fullWidth
            required
            value={justification}
            onChange={e => {
              setJustification(e.target.value);
              changed();
            }}
          />
          <TextField
            id={`${fieldId}-desired`}
            label="Desired state (JSON)"
            helperText={
              op === 'promote' || op === 'rollback'
                ? 'Specify releaseName for the target environment.'
                : op === 'resource-update'
                ? 'Specify resourceRelease, resourceTypeEnvironmentConfigs or retainPolicy.'
                : 'Use the desired payload supported by the operation handler.'
            }
            fullWidth
            multiline
            value={desired}
            onChange={e => {
              setDesired(e.target.value);
              changed();
            }}
          />
          <ErrorMessage error={error} />
        </DialogContent>
        <DialogActions>
          <Button disabled={busy} onClick={() => setOpen(false)}>
            Cancel
          </Button>
          <Button
            disabled={busy || !justification.trim() || !environment.trim()}
            onClick={submit}
          >
            Submit request
          </Button>
        </DialogActions>
      </Dialog>
    </>
  );
}
export function ResourceCard() {
  const { entity } = useEntity();
  const api = useApi(aepApiRef);
  const ctx = useSession();
  const [services, setServices] = useState<Service[]>([]);
  const [error, setError] = useState('');
  const annotations = entity.metadata.annotations ?? {};
  const organization =
    annotations['aep.aenix.io/organization'] ??
    annotations['openchoreo.io/namespace'] ??
    '';
  const project =
    annotations['openchoreo.io/project'] ??
    annotations['openchoreo.io/project-name'] ??
    '';
  const name =
    annotations['openchoreo.io/resource'] ??
    annotations['openchoreo.io/resource-name'] ??
    entity.metadata.name;
  const member = ctx.session?.organizations.some(o => o.name === organization);
  useEffect(() => {
    let active = true;
    setServices([]);
    if (member)
      api
        .services(organization)
        .then(r => {
          if (active)
            setServices(
              r.items.filter(
                s =>
                  s.labels['aep.aenix.io/resource'] === name &&
                  s.labels['aep.aenix.io/project'] === project,
              ),
            );
        })
        .catch(e => {
          if (active) setError(e.message);
        });
    return () => {
      active = false;
    };
  }, [api, organization, member, name, project]);
  return (
    <InfoCard title="Cozystack resource">
      <ErrorMessage error={ctx.error || error} />
      {services.map(s => (
        <section key={s.name}>
          <ServiceSummary service={s} />
          {['resource-restore', 'credential-rotate', 'resource-update'].map(
            op => (
              <RequestChange
                key={op}
                organization={organization}
                target={{
                  kind: 'Resource',
                  name,
                  project,
                  environment: s.labels['aep.aenix.io/environment'],
                }}
                operation={op}
              />
            ),
          )}
        </section>
      ))}
      {!services.length && <p>No accessible ServiceClaim binding.</p>}
    </InfoCard>
  );
}
export function EntityChangeCard() {
  const { entity } = useEntity();
  const a = entity.metadata.annotations ?? {};
  const ctx = useSession();
  const org =
    a['aep.aenix.io/organization'] ?? a['openchoreo.io/namespace'] ?? '';
  return (
    <InfoCard title="Protected environment changes">
      <p>
        Request promotion, rollback or a reviewed suggestion. Protected writes
        require approval.
      </p>
      <RequestChange
        organization={
          ctx.session?.organizations.some(o => o.name === org) ? org : ''
        }
        target={{
          kind: entity.kind === 'System' ? 'Project' : entity.kind,
          name:
            a['openchoreo.io/component'] ??
            a['openchoreo.io/resource'] ??
            a['openchoreo.io/project'] ??
            entity.metadata.name,
          project:
            entity.kind === 'System' ? undefined : a['openchoreo.io/project'],
        }}
      />
    </InfoCard>
  );
}
export function ChangesPage() {
  const api = useApi(aepApiRef);
  const ctx = useSession();
  const [items, setItems] = useState<Change[]>([]);
  const [selected, setSelected] = useState<Change>();
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const approver = ctx.session?.organizations
    .find(o => o.name === ctx.org)
    ?.roles.includes('approver');
  useEffect(() => {
    let active = true;
    setItems([]);
    setSelected(undefined);
    setError('');
    if (ctx.org && approver)
      api
        .changes(ctx.org)
        .then(r => {
          if (active) setItems(r.items);
        })
        .catch(e => {
          if (active) setError(e.message);
        });
    return () => {
      active = false;
    };
  }, [api, ctx.org, approver]);
  async function detail(name: string) {
    setError('');
    try {
      setSelected((await api.change(ctx.org, name)).request);
    } catch (e) {
      setError((e as Error).message);
    }
  }
  async function decide(decision: string) {
    if (!selected) return;
    setBusy(true);
    setError('');
    try {
      setSelected((await api.decide(selected, decision)).request);
      setItems((await api.changes(ctx.org)).items);
    } catch (e) {
      setSelected(undefined);
      setError(
        `${(e as Error).message}. Reload the change before deciding again.`,
      );
    } finally {
      setBusy(false);
    }
  }
  return (
    <Page themeId="tool">
      <Header title="Changes" />
      <Content>
        <OrgSelect session={ctx.session} org={ctx.org} onChange={ctx.setOrg} />
        <ErrorMessage error={ctx.error || error} />
        {!approver ? (
          <p>The approver role is required.</p>
        ) : (
          <>
            <ul>
              {items.map(c => (
                <li key={c.metadata.name}>
                  <Button onClick={() => detail(c.metadata.name)}>
                    {c.metadata.name}
                  </Button>{' '}
                  {c.spec.requester} — {c.status?.phase ?? 'Draft'}
                </li>
              ))}
            </ul>
            {!items.length && <p>No changes.</p>}
            {selected && (
              <section>
                <h2>{selected.metadata.name}</h2>
                <p>Requested by: {selected.spec.requester}</p>
                <p>
                  {selected.spec.operation} —{' '}
                  {selected.status?.phase ?? 'Draft'}
                </p>
                <p>{selected.spec.justification}</p>
                <p>
                  Window:{' '}
                  {selected.spec.window
                    ? `${selected.spec.window.start} – ${selected.spec.window.end}`
                    : 'Default expiry'}
                </p>
                <ul>
                  {selected.status?.approvals?.map(a => (
                    <li key={a.user}>
                      {a.user}: {a.decision} ({a.role}) — {a.time}
                    </li>
                  ))}
                </ul>
                {['approve', 'reject'].map(d => (
                  <Button
                    key={d}
                    disabled={
                      busy ||
                      selected.status?.phase !== 'Pending' ||
                      selected.spec.requester === ctx.session?.username
                    }
                    onClick={() => decide(d)}
                  >
                    {d === 'approve' ? 'Approve' : 'Reject'}
                  </Button>
                ))}
              </section>
            )}
          </>
        )}
      </Content>
    </Page>
  );
}
