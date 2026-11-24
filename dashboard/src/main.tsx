import { render } from 'preact';
import { App } from './app';
import { boot } from './state';
import './styles/tokens.css';
import './styles/app.css';

const root = document.getElementById('app')!;
root.textContent = '';
render(<App />, root);
void boot();
