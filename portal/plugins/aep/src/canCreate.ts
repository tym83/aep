import { useEffect, useState } from 'react';
import { useApi } from '@backstage/core-plugin-api';
import { aepApiRef } from './api';

// Roles that may create projects, components or resources in an organization.
const creatorRoles = ['developer', 'platform', 'admin'];

/**
 * Whether the signed-in user may create anything in at least one
 * organization. Unknown (loading or failed) counts as no, so read-only roles
 * such as approver, auditor and finance never see creation entry points.
 */
export function useCanCreate(): boolean {
  const api = useApi(aepApiRef);
  const [canCreate, setCanCreate] = useState(false);
  useEffect(() => {
    let active = true;
    api
      .session()
      .then(s => {
        if (active)
          setCanCreate(
            s.organizations.some(o =>
              o.roles.some(r => creatorRoles.includes(r)),
            ),
          );
      })
      .catch(() => active && setCanCreate(false));
    return () => {
      active = false;
    };
  }, [api]);
  return canCreate;
}
