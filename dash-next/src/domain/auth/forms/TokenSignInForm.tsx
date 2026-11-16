import { useForm } from '@tanstack/react-form';

import { useLogin } from '~/api/dashboard/dashboardComponents';
import { useServerConfig } from '~/shared/context/ConfigContext';

import { CredentialField } from '../components/CredentialField/CredentialField';
import { SignInForm } from '../components/SignInForm/SignInForm';
import { sessionScope, signInMutationKey } from '../constants';
import { useSession } from '../hooks/useSession';
import { tokenSignInSchema } from '../schemas/signIn';
import { toSignInNotice } from '../utils/formatters';
import { hasUser, isTokenBlank } from '../utils/predicates';
import { fromTokenLogin, toTokenLogin } from '../utils/transforms';

const tokenHint = 'Create one with kubectl create token <serviceaccount> -n <namespace>.';

export const TokenSignInForm = () => {
  const config = useServerConfig();
  const { onSignedIn, onIdentityMissing } = useSession();
  const login = useLogin({
    mutationKey: signInMutationKey,
    scope: sessionScope,
    onSuccess: (answer) =>
      hasUser(answer) ? onSignedIn(fromTokenLogin(answer)) : onIdentityMissing(),
  });
  const form = useForm({
    defaultValues: { token: '' },
    validators: { onSubmit: tokenSignInSchema },
    onSubmit: ({ value }) => {
      login.mutate({ body: toTokenLogin(value) });
    },
  });
  const failure = toSignInNotice(login, config);

  return (
    <form.Subscribe selector={(state) => state.values.token}>
      {(token) => (
        <SignInForm
          failure={failure}
          isPending={login.isPending}
          isIncomplete={isTokenBlank(token)}
          onSubmit={() => void form.handleSubmit()}
        >
          <form.Field name="token">
            {(field) => (
              <CredentialField
                field={field}
                label="Service account token"
                type="password"
                autoComplete="off"
                hint={tokenHint}
                isLocked={login.isPending}
              />
            )}
          </form.Field>
        </SignInForm>
      )}
    </form.Subscribe>
  );
};
