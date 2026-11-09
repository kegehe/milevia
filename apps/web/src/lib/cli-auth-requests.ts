/** 登录读数按环境/工具保留最新请求，面板按每次打开或操作作废旧响应。 */
export class CLIAuthRequests {
  private readings = new Map<string, symbol>();
  private panel = Symbol();

  beginReading(runnerID: string, agentID: string): () => boolean {
    const key = JSON.stringify([runnerID, agentID]);
    const token = Symbol();
    this.readings.set(key, token);
    return () => this.readings.get(key) === token;
  }

  beginPanel(): () => boolean {
    const token = Symbol();
    this.panel = token;
    return () => this.panel === token;
  }

  closePanel(): void {
    this.panel = Symbol();
  }
}
