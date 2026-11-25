import {
  coreServices,
  createBackendPlugin,
} from '@backstage/backend-plugin-api';
import { createRouter } from './router';

export default createBackendPlugin({
  pluginId: 'aep',
  register(env) {
    env.registerInit({
      deps: {
        config: coreServices.rootConfig,
        httpRouter: coreServices.httpRouter,
        httpAuth: coreServices.httpAuth,
      },
      async init({ config, httpRouter, httpAuth }) {
        httpRouter.use(async (req, _res, next) => {
          try {
            await httpAuth.credentials(req, { allow: ['user'] });
            next();
          } catch (error) {
            next(error);
          }
        });
        httpRouter.use(
          createRouter({
            readApiUrl: config.getString('aep.readApiUrl'),
            changeApiUrl: config.getString('aep.changeApiUrl'),
          }),
        );
      },
    });
  },
});
