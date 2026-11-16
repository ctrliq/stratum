import { Tooltip } from '@ctrliq/quantic-react';

type Props = {
  text: string;
  hint: string | undefined;
};

export function HintedText({ text, hint }: Props) {
  return hint ? (
    <Tooltip>
      <span>{text}</span>
      <span slot="content">{hint}</span>
    </Tooltip>
  ) : (
    text
  );
}
