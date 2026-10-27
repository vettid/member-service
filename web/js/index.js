// The landing page: a signed-in member goes straight home.

import { getOrNull } from './api.js';
import { param } from './dom.js';
import * as ui from './ui.js';

ui.init();

if (param('signedout') === '1') {
  ui.showNotice('You have signed out.', 'info');
  history.replaceState(null, '', location.pathname);
} else {
  getOrNull('/api/me').then(
    (me) => {
      if (me) location.replace('/home/');
    },
    () => {},
  );
}
