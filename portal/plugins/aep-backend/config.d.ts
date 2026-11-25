export interface Config {
  aep: {
    /** URL of the authenticated organization read service. */
    readApiUrl: string;
    /** URL of the authenticated change service. */
    changeApiUrl: string;
  };
}
