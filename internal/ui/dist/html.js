// html.js —— 全站共用一个 htm 绑定实例。
import { h } from 'preact';
import htm from 'htm';

export const html = htm.bind(h);
