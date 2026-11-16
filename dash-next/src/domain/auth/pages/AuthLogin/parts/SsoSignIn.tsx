import { ButtonGrow, ButtonVariant } from '@ctrliq/quantic-components';
import { Button } from '@ctrliq/quantic-react';
import { useIsMutating } from '@tanstack/react-query';

import { signInMutationKey } from '../../../constants';
import type { ReturnTarget } from '../../../types/session';
import { toOidcStartUrl } from '../../../utils/transforms';

type Props = {
  returnTarget: ReturnTarget | undefined;
};

export const SsoSignIn = ({ returnTarget }: Props) => {
  const isSigningIn = useIsMutating({ mutationKey: signInMutationKey }) > 0;
  return (
    <Button
      variant={ButtonVariant.Secondary}
      grow={ButtonGrow.Always}
      disabled={isSigningIn}
      onClick={() => {
        window.location.assign(toOidcStartUrl(returnTarget));
      }}
    >
      Sign in with SSO
    </Button>
  );
};
