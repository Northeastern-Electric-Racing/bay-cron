import { defineJob } from '../types.js';

export default defineJob({
  name: 'meeting-attendance',
  schedule: '*/5 * * * *',
  endpoint: '/notifications/attendance'
});