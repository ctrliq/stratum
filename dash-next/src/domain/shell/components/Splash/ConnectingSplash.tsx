import { useServerConfig } from '~/shared/context/ConfigContext';

import { toSplash } from '../../utils/formatters';
import { Splash } from './Splash';

export const ConnectingSplash = () => <Splash>{toSplash(useServerConfig())}</Splash>;
