import { rootHttpRouterServiceFactory } from '@backstage/backend-defaults/rootHttpRouter';
import {
  immediateCatalogServiceFactory,
  annotationStoreFactory,
} from '@openchoreo/backstage-plugin-catalog-backend-module';
import { createIdpTokenHeaderMiddleware } from '@openchoreo/openchoreo-auth';
import { OpenChoreoAuthModule } from '@openchoreo/backstage-plugin-auth-backend-module-openchoreo-auth';

import { createBackend } from '@backstage/backend-defaults';

const backend = createBackend();

backend.add(import('@backstage/plugin-app-backend'));
backend.add(import('@backstage/plugin-proxy-backend'));

// scaffolder plugin
backend.add(import('@backstage/plugin-scaffolder-backend'));
backend.add(import('@backstage/plugin-scaffolder-backend-module-github'));
backend.add(
  import('@backstage/plugin-scaffolder-backend-module-notifications'),
);

// techdocs plugin
backend.add(import('@backstage/plugin-techdocs-backend'));

// auth plugin
backend.add(import('@backstage/plugin-auth-backend'));
// See https://backstage.io/docs/backend-system/building-backends/migrating#the-auth-plugin

// catalog plugin
backend.add(import('@backstage/plugin-catalog-backend'));
backend.add(
  import('@backstage/plugin-catalog-backend-module-scaffolder-entity-model'),
);

// See https://backstage.io/docs/features/software-catalog/configuration#subscribing-to-catalog-errors
backend.add(import('@backstage/plugin-catalog-backend-module-logs'));

// permission plugin
backend.add(import('@backstage/plugin-permission-backend'));
// See https://backstage.io/docs/permissions/getting-started for how to create your own permission policy

// search plugin
backend.add(import('@backstage/plugin-search-backend'));

// search engine
// See https://backstage.io/docs/features/search/search-engines
backend.add(import('@backstage/plugin-search-backend-module-pg'));

// search collators
backend.add(import('@backstage/plugin-search-backend-module-catalog'));
backend.add(import('@backstage/plugin-search-backend-module-techdocs'));

// kubernetes plugin
backend.add(import('@backstage/plugin-kubernetes-backend'));

// notifications and signals plugins
backend.add(import('@backstage/plugin-notifications-backend'));
backend.add(import('@backstage/plugin-signals-backend'));

backend.add(immediateCatalogServiceFactory);
backend.add(annotationStoreFactory);
backend.add(
  rootHttpRouterServiceFactory({
    configure: ({ app, applyDefaults, middleware }) => {
      app.use(middleware.helmet());
      app.use(middleware.cors());
      app.use(middleware.compression());
      app.use(createIdpTokenHeaderMiddleware());
      applyDefaults();
    },
  }),
);
backend.add(OpenChoreoAuthModule);
backend.add(
  import(
    '@openchoreo/backstage-plugin-permission-backend-module-openchoreo-policy'
  ),
);
backend.add(import('@openchoreo/backstage-plugin-catalog-backend-module'));
backend.add(import('@openchoreo/backstage-plugin-backend'));
backend.add(import('@openchoreo/backstage-plugin-scaffolder-backend-module'));
backend.add(import('@internal/plugin-aep-backend'));
backend.start();
