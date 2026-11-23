import '@testing-library/jest-dom/vitest';
import { cleanup } from '@testing-library/react';
import { afterEach } from 'vitest';

// 每个测试结束后自动卸载 React 树，防止不同用例的 DOM 互相污染。
afterEach(() => {
  cleanup();
});
