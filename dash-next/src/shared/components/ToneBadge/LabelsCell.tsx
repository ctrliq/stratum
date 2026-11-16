import { Tag, TagGroup } from '@ctrliq/quantic-react';

type Props = {
  getValue: () => unknown;
};

const labelSeparator = ', ';

export function LabelsCell({ getValue }: Props) {
  const labels = String(getValue()).split(labelSeparator).filter(Boolean);
  return labels.length === 0 ? null : (
    <TagGroup aria-label="Labels">
      {labels.map((label) => (
        <Tag key={label} size="sm">
          {label}
        </Tag>
      ))}
    </TagGroup>
  );
}
