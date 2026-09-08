"""Usage: python3 clean_access_log.py access_log.csv access_log_clean.csv"""
import sys

def clean(infile, outfile):
    good, bad = 0, 0
    header = "timestamp,operation,key,hit,size_bytes,ttl_remaining,cache_used\n"
    with open(infile, 'rb') as f, open(outfile, 'w') as out:
        out.write(header)
        for raw in f:
            line = raw.decode('utf-8', errors='ignore').replace('\x00', '').strip()
            if not line or line == header.strip():
                continue
            if len(line.split(',')) == 7:
                out.write(line + '\n')
                good += 1
            else:
                bad += 1
    print(f"kept={good} dropped={bad} -> {outfile}")

if __name__ == '__main__':
    infile = sys.argv[1] if len(sys.argv) > 1 else 'access_log.csv'
    outfile = sys.argv[2] if len(sys.argv) > 2 else 'access_log_clean.csv'
    clean(infile, outfile)
