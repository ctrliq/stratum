import { useForm } from '@tanstack/react-form';

import { useLogin } from '~/api/fleet/fleetComponents';
import { useServerConfig } from '~/shared/context/ConfigContext';

import { CredentialField } from '../components/CredentialField/CredentialField';
import { SignInForm } from '../components/SignInForm/SignInForm';
import { sessionScope, signInMutationKey } from '../constants';
import { useSession } from '../hooks/useSession';
import { passwordSignInSchema } from '../schemas/signIn';
import { toSignInNotice } from '../utils/formatters';
import { hasUser, isCredentialMissing } from '../utils/predicates';
import { fromPasswordLogin, toPasswordLogin } from '../utils/transforms';

export const PasswordSignInForm = () => {
  const config = useServerConfig();
  const { onSignedIn, onIdentityMissing } = useSession();
  const login = useLogin({
    mutationKey: signInMutationKey,
    scope: sessionScope,
    onSuccess: (answer) =>
      hasUser(answer) ? onSignedIn(fromPasswordLogin(answer)) : onIdentityMissing(),
  });
  const form = useForm({
    defaultValues: { username: '', password: '' },
    validators: { onSubmit: passwordSignInSchema },
    onSubmit: ({ value }) => {
      login.mutate({ body: toPasswordLogin(value) });
    },
  });
  const failure = toSignInNotice(login, config);

  return (
    <form.Subscribe selector={(state) => state.values}>
      {(credential) => (
        <SignInForm
          failure={failure}
          isPending={login.isPending}
          isIncomplete={isCredentialMissing(credential)}
          onSubmit={() => void form.handleSubmit()}
        >
          <form.Field name="username">
            {(field) => (
              <CredentialField
                field={field}
                label="Username"
                type="text"
                autoComplete="username"
                isLocked={login.isPending}
              />
            )}
          </form.Field>
          <form.Field name="password">
            {(field) => (
              <CredentialField
                field={field}
                label="Password"
                type="password"
                autoComplete="current-password"
                isLocked={login.isPending}
              />
            )}
          </form.Field>
        </SignInForm>
      )}
    </form.Subscribe>
  );
};
