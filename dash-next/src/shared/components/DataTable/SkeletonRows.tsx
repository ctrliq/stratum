import { Skeleton, TableBody, TableCell, TableRow } from '@ctrliq/quantic-react';

type Props = {
  columnCount: number;
  rowCount?: number;
};

export function SkeletonRows({ columnCount, rowCount = 5 }: Props) {
  return (
    <TableBody>
      {Array.from({ length: rowCount }, (_, rowIndex) => (
        <TableRow key={rowIndex}>
          {Array.from({ length: columnCount }, (_, colIndex) => (
            <TableCell key={colIndex}>
              <Skeleton />
            </TableCell>
          ))}
        </TableRow>
      ))}
    </TableBody>
  );
}
