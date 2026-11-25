import { createApp } from '@backstage/frontend-defaults';
import catalogPlugin from '@backstage/plugin-catalog/alpha';
import openchoreoPlugin, {
  openChoreoEntityGroupsModule,
} from '@openchoreo/backstage-plugin/alpha';
import aepPlugin, { aepChangeModule } from '@internal/plugin-aep';
import { navModule } from './modules/nav';
import { signInModule } from './signInModule';
export default createApp({
  features: [
    catalogPlugin,
    openchoreoPlugin,
    openChoreoEntityGroupsModule,
    aepPlugin,
    aepChangeModule,
    navModule,
    signInModule,
  ],
});
