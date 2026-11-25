import {
  ApiBlueprint,
  PageBlueprint,
  createFrontendPlugin,
  createFrontendModule,
  discoveryApiRef,
  fetchApiRef,
  createRouteRef,
} from '@backstage/frontend-plugin-api';
import { EntityCardBlueprint } from '@backstage/plugin-catalog-react/alpha';
import { compatWrapper } from '@backstage/core-compat-api';
import StorageIcon from '@material-ui/icons/Storage';
import AssignmentIcon from '@material-ui/icons/Assignment';
import { openChoreoAuthApiRef } from '@openchoreo/backstage-plugin';
import { AepApi, aepApiRef } from './api';
const infrastructure = createRouteRef();
const changes = createRouteRef();
export default createFrontendPlugin({
  pluginId: 'aep',
  extensions: [
    ApiBlueprint.make({
      name: 'api',
      params: defineParams =>
        defineParams({
          api: aepApiRef,
          deps: {
            discovery: discoveryApiRef,
            fetchApi: fetchApiRef,
            auth: openChoreoAuthApiRef,
          },
          factory: ({ discovery, fetchApi, auth }) =>
            new AepApi(discovery, fetchApi, auth),
        }),
    }),
    PageBlueprint.make({
      name: 'infrastructure',
      params: {
        path: '/aep/infrastructure',
        title: 'Organization infrastructure',
        icon: <StorageIcon />,
        noHeader: true,
        routeRef: infrastructure,
        loader: () =>
          import('./components').then(m =>
            compatWrapper(<m.InfrastructurePage />),
          ),
      },
    }),
    PageBlueprint.make({
      name: 'changes',
      params: {
        path: '/aep/changes',
        title: 'Changes',
        icon: <AssignmentIcon />,
        noHeader: true,
        routeRef: changes,
        loader: () =>
          import('./components').then(m => compatWrapper(<m.ChangesPage />)),
      },
    }),
    EntityCardBlueprint.make({
      name: 'resource',
      params: {
        filter: e => e.kind.toLowerCase() === 'resource',
        loader: () => import('./components').then(m => <m.ResourceCard />),
      },
    }),
  ],
});
// OpenChoreo m.2 exposes whole cards/tabs, not individual mutation actions.
// Add the documented fallback through an NFS module without hiding non-production tabs.
export const aepChangeModule = createFrontendModule({
  // OpenChoreo bespoke layouts omit cards owned by openchoreo; AEP cards are retained.
  pluginId: 'aep',
  extensions: [
    EntityCardBlueprint.make({
      name: 'aep-request-change',
      params: {
        filter: e =>
          ['component', 'resource', 'system'].includes(e.kind.toLowerCase()),
        loader: () => import('./components').then(m => <m.EntityChangeCard />),
      },
    }),
  ],
});
