import { useEffect, useState } from 'react';

const tickMs = 10_000;

export const useNow = (): number => {
  const [now, setNow] = useState(Date.now);

  useEffect(() => {
    const ticking = setInterval(() => {
      setNow(Date.now());
    }, tickMs);
    return () => {
      clearInterval(ticking);
    };
  }, []);

  return now;
};
