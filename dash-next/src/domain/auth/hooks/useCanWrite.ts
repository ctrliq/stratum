import { useServerConfig } from '~/shared/context/ConfigContext';

import { isWriteOffered } from '../utils/predicates';
import { identityOf } from '../utils/selectors';
import { useSession } from './useSession';

export const useCanWrite = (): boolean => {
  const config = useServerConfig();
  const { session, isChanging } = useSession();
  return isWriteOffered(config, identityOf(session), isChanging);
};
