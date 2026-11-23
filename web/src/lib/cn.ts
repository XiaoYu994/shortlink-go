import { type ClassValue, clsx } from 'clsx';
import { twMerge } from 'tailwind-merge';

/*
  cn 是 shadcn/ui 的标准工具函数：
  1. clsx 负责条件拼接 className（类似数组过滤掉 false/undefined）；
  2. tailwind-merge 负责解决冲突类（例如组件默认 "px-4"，调用方传 "px-2" 时自动保留后者）。
*/
export function cn(...inputs: ClassValue[]): string {
  return twMerge(clsx(inputs));
}
