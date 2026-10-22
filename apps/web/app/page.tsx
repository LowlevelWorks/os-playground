import { Playground } from "@/components/Playground";

export default function Page() {
  return (
    <main className="page">
      <header className="intro">
        <h1>OS Playground</h1>
        <p>
          Start a session to boot your own copy of the operating system. It runs in an isolated
          virtual machine and is thrown away when you end the session or leave it idle.
        </p>
      </header>
      <Playground />
    </main>
  );
}

